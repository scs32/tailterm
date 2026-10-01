import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,readdirSync,rmSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname} from "node:path";
import {execFileSync,spawn} from "node:child_process";
import {integrateCandidate,publishIntegration,runRelease,liveCheck,runnableJob,HostAdapter,serveDeployment,reconcileHostLocks,revertCommit,moveReleaseRef,tasksHubCheckedOut,releaseError,failureReason,MATRIX_PREREQUISITES,MATRIX_RUN_PATTERN,MATRIX_HOST_WAIT_MS,missingPrerequisites,provisionPrerequisites,matrixRunTimeout} from "../scripts/release-runner.mjs";
import {createHash} from "node:crypto";
import {renderTeamDelivery} from "../client/team-delivery-view.js";
import {targetsForPaths,selectReleaseTargets} from "../scripts/release-targets.mjs";
const git=(cwd,...args)=>execFileSync("git",args,{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"]}).trim();
function fixture(){const cwd=mkdtempSync(join(tmpdir(),"release-git-")),origin=mkdtempSync(join(tmpdir(),"release-origin-"));git(origin,"init","--bare","-b","tasks-hub");git(cwd,"init","-b","tasks-hub");git(cwd,"config","user.email","fixture@example.invalid");git(cwd,"config","user.name","Fixture");mkdirSync(join(cwd,"client"));writeFileSync(join(cwd,"client/base.js"),"base");git(cwd,"add",".");git(cwd,"commit","-m","base");const base=git(cwd,"rev-parse","HEAD");git(cwd,"remote","add","origin",origin);git(cwd,"push","--quiet","origin","tasks-hub");git(cwd,"checkout","-b","candidate");return {cwd,base,origin};}
function change(f,file,text){writeFileSync(join(f.cwd,file),text);git(f.cwd,"add",".");git(f.cwd,"commit","-m","candidate");return git(f.cwd,"rev-parse","HEAD");}
function job(f,commit){return {id:"rel_fixture",state:"claimed",commit,baseCommit:f.base,verificationDigest:"a".repeat(64),plan:{commit}};}
test("fast forward pins exact candidate and ref CAS refuses a race",()=>{const f=fixture();const commit=change(f,"client/a.js","a");const out=integrateCandidate(f.cwd,job(f,commit));assert.equal(out.integrated,commit);git(f.cwd,"update-ref","refs/heads/tasks-hub",commit,f.base);assert.throws(()=>publishIntegration(f.cwd,commit,f.base),/race/);});
test("full divergent series is cherry picked and conflict leaves ref unchanged",()=>{const f=fixture();change(f,"client/a.js","a");const commit=change(f,"client/b.js","b");git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");const prior=git(f.cwd,"rev-parse","HEAD");const out=integrateCandidate(f.cwd,job(f,commit));assert.notEqual(out.integrated,commit);assert.equal(readFileSync(join(f.cwd,"client/b.js"),"utf8"),"b");assert.equal(git(f.cwd,"rev-parse","tasks-hub"),prior);
const g=fixture();const conflicting=change(g,"client/base.js","candidate");git(g.cwd,"checkout","tasks-hub");change(g,"client/base.js","release");assert.throws(()=>integrateCandidate(g.cwd,job(g,conflicting)),/refused/);assert.equal(git(g.cwd,"status","--porcelain"),"");});
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

test("host Mini adapter uses atomic install and retained rollback without changing fixture database",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"mini-adapter-")),install=join(cwd,"tt"),artifact=join(cwd,"next"),backup=join(cwd,"before"),db=join(cwd,"fixture.sqlite");
 writeFileSync(install,"old CLI");writeFileSync(artifact,"new CLI");writeFileSync(db,"newer writes");
 const a={installPath:install,artifactPath:artifact,rollbackPath:backup,rollbackSafe:true,artifactSHA256:hash(readFileSync(artifact)),relayRestart:[process.execPath,"-e","process.exit(0)"],rollbackProbe:[process.execPath,"-e","console.log(JSON.stringify({restored:true,databaseWritesPreserved:true}))"]};
 const adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.artifacts.set("mini",a);
 adapter.captureMiniRollback(a);adapter.artifacts.set("mini",a);
 await adapter.deploy("mini",a);assert.equal(readFileSync(install,"utf8"),"new CLI");assert.equal(readFileSync(a.rollbackPath,"utf8"),"old CLI");
 assert.equal(await adapter.rollback("mini"),true);assert.equal(readFileSync(install,"utf8"),"old CLI");assert.equal(readFileSync(db,"utf8"),"newer writes");
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
 const config={cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),targets:{mini:{installPath:install,rollbackPath:join(home,"stale-before"),release:"stale-release",relayRestart:[process.execPath,"-e","process.exit(0)"],rollbackProbe:[process.execPath,"-e","console.log(JSON.stringify({restored:true,databaseWritesPreserved:true}))"]}}};
 return {f,commit,home,install,config};
}
test("b1 two consecutive jobs share stable config and restore the exact prior-live Mini",async()=>{
 const {f,commit,home,install,config}=hostFixture();
 for(const [id,version] of [["rel_first","v2"],["rel_second","v3"]]){
  const adapter=new HostAdapter(config,{...job(f,commit),id});const release=id+"-"+commit.slice(0,12)+"-mini";
  const manifest=importInputs(adapter,commit,{mini:{release,rollbackSafe:true}});
  let restarts=0;
  adapter.command=(argv)=>{if(argv[1]==="version")return stamped(commit);if(argv.includes("build")){writeFileSync(argv[argv.indexOf("-o")+1],version);return "";}if(id==="rel_second" && argv.includes("process.exit(0)") && ++restarts===1)throw new Error("Synthetic second relay restart failure");return JSON.stringify({restored:true,databaseWritesPreserved:true});};
  const artifact=await adapter.prepare("mini",commit);
  if(id==="rel_second")await assert.rejects(adapter.deploy("mini",artifact),/Synthetic second/);else await adapter.deploy("mini",artifact);
  assert.equal(readFileSync(install,"utf8"),version);
  if(id==="rel_second"){assert.equal(readFileSync(artifact.rollbackPath,"utf8"),"v2");assert.equal(await adapter.rollback("mini"),true);assert.equal(readFileSync(install,"utf8"),"v2");}
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
const attemptDir=(home,commit,n)=>join(home,"rel_fixture-integrated-verification",`${commit}-r${n}`);
function matrixHost({plan=workedPlan,receipt={environment:{},checks:[{exitCode:0}]},jobs=[]}={}){
 const f=fixture(),home=mkdtempSync(join(tmpdir(),"matrix-host-"));ignorePrerequisites(f.cwd);placePrerequisites(f.cwd);
 const adapter=new HostAdapter({cwd:f.cwd,journalDirectory:home},{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});
 const calls=[];adapter.matrixRunsActive=()=>0;
 adapter.command=(argv,cwd,options)=>{calls.push({argv,timeout:options?.timeout??600000});
  if(argv[1]==="deployment"&&argv[2]==="list")return JSON.stringify(jobs);
  if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123abcd"});
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){writeFileSync(argv[4],JSON.stringify(plan));return "";}
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="run"){writeFileSync(join(argv[4],"receipt.json"),JSON.stringify(receipt));return "";}
  return "";};
 const integrated={id:"rel_fixture",integratedCommit:"c".repeat(40),plan:{commit:"a".repeat(40)}};
 return {f,home,adapter,calls,integrated,matrix:()=>calls.filter(c=>c.argv[1]==="scripts/verify-matrix.mjs"),sends:()=>calls.filter(c=>c.argv[1]==="send")};
}
test("p1 a missing matrix prerequisite refuses the release by name before any matrix run",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a"));git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");
 ignorePrerequisites(f.cwd);placePrerequisites(f.cwd,[".build/test.wasm"]);
 const c=config(f,j),a=fake(),argvs=[];let escalation;
 const host=new HostAdapter({cwd:f.cwd,journalDirectory:dirname(c.journalPath)},{id:"rel_fixture"});host.command=argv=>{argvs.push(argv);return argv[2]==="list"?"[]":"";};host.matrixRunsActive=()=>0;
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
 const source=readFileSync(new URL("../scripts/verify-matrix.mjs",import.meta.url),"utf8"),at=source.indexOf('"Missing prerequisite: "');
 const start=source.lastIndexOf("for (const p of [",at),end=source.indexOf("])",start);assert.ok(at>0&&start>0&&end<at);
 assert.deepEqual([...source.slice(start,end).matchAll(/"([^"]+)"/g)].map(m=>m[1]),MATRIX_PREREQUISITES);
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
test("p2 the matrix run gets a timeout derived from the plan and other commands keep the command limit",async()=>{
 assert.equal(matrixRunTimeout(workedPlan),7560000);
 assert.equal(matrixRunTimeout({checks:[{environment:{VERIFICATION_TIMEOUT_MS:"600000"}}]}),2400000,"a plan without maxAttempts runs each check once");
 for(const bad of [{...workedPlan,checks:[]},{checks:[{environment:{}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:"1800001"}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:"0"}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:1800000}}]},{maxAttempts:0,checks:workedPlan.checks}])
  assert.throws(()=>matrixRunTimeout(bad),e=>/^(Matrix plan has no checks|Invalid matrix check timeout|Invalid matrix attempt limit)$/.test(failureReason(e)));
 const h=matrixHost();assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
 const matrix=h.matrix();assert.deepEqual(matrix.map(c=>[c.argv[2],c.timeout]),[["plan",600000],["run",7560000]]);
 assert.ok(h.calls.filter(c=>c.argv[2]!=="run").every(c=>c.timeout===600000));assert.equal(h.sends().length,1);
 const bad=matrixHost({plan:{maxAttempts:3,checks:[{environment:{VERIFICATION_TIMEOUT_MS:"3600000"}}]}});
 await assert.rejects(bad.adapter.verifyIntegrated(bad.integrated),e=>failureReason(e)==="Invalid matrix check timeout");assert.deepEqual(bad.matrix().map(c=>c.argv[2]),["plan"]);
 const failing=matrixHost({receipt:{environment:{},checks:[{exitCode:1}]}});
 await assert.rejects(failing.adapter.verifyIntegrated(failing.integrated),e=>failureReason(e)==="Integrated matrix receipt is not eligible");assert.equal(failing.sends().length,0);
 const real=new HostAdapter({cwd:tmpdir()},{});assert.throws(()=>real.command([process.execPath,"-e","setTimeout(()=>{},5000)"],tmpdir(),{timeout:200}),e=>/ timeout$/.test(failureReason(e)));
 assert.equal(real.command([process.execPath,"-e","console.log('ok')"]).trim(),"ok");
});
test("p2 the host probe counts only real node matrix runs",async()=>{
 const re=new RegExp(MATRIX_RUN_PATTERN);
 for(const line of ["node scripts/verify-matrix.mjs run p d","/opt/homebrew/bin/node /abs/scripts/verify-matrix.mjs targeted c d","node /Users/x/tailterm/scripts/verify-matrix.mjs run /tmp/plan.json /tmp/out --jobs 4"])assert.match(line,re,line);
 for(const line of ["claude --model opus --append-system-prompt Run node scripts/verify-matrix.mjs run PLAN_JSON LOG_DIR","node scripts/verify-matrix.mjs plan c p","codex exec node scripts/verify-matrix.mjs run p d","node -e x node scripts/verify-matrix.mjs run p d","node scripts/release-runner.mjs --config /private/c.json"])assert.doesNotMatch(line,re,line);
 // The same pattern through the real pgrep, limited to this test's children.
 const dir=mkdtempSync(join(tmpdir(),"matrix-probe-"));mkdirSync(join(dir,"scripts"));writeFileSync(join(dir,"scripts/verify-matrix.mjs"),"console.log('ready');setTimeout(()=>{},20000);");
 const start=argv=>new Promise((resolveStart,reject)=>{const child=spawn(argv[0],argv.slice(1),{stdio:["ignore","pipe","ignore"]});child.on("error",reject);child.stdout.once("data",()=>resolveStart(child));});
 const own=(file,args,options)=>execFileSync(file,["-P",String(process.pid),...args],options);
 const adapter=new HostAdapter({cwd:dir},{}),children=[];
 try{
  children.push(await start([process.execPath,"-e","console.log('ready');setTimeout(()=>{},20000)","node scripts/verify-matrix.mjs run PLAN_JSON LOG_DIR"]));
  children.push(await start([process.execPath,join(dir,"scripts/verify-matrix.mjs"),"plan","c","p"]));
  assert.equal(adapter.matrixRunsActive(own),0,"prompt text and plan mode are not runs");
  children.push(await start([process.execPath,join(dir,"scripts/verify-matrix.mjs"),"run","p","d"]));
  assert.equal(adapter.matrixRunsActive(own),1);
 }finally{for(const child of children)child.kill();}
 assert.throws(()=>adapter.matrixRunsActive(()=>{const e=new Error("pgrep");e.status=2;throw e;}),e=>failureReason(e)==="Matrix host probe unavailable");
 assert.equal(adapter.matrixRunsActive(()=>{const e=new Error("none");e.status=1;throw e;}),0);
 const source=readFileSync(RUNNER,"utf8");assert.doesNotMatch(source,/process\.kill|pkill|"kill"/);
});
test("p2 the integrated run waits for other matrix runs and refuses by name after the bound",async()=>{
 const h=matrixHost();let busy=1,now=1000;h.adapter.matrixRunsActive=()=>busy;h.adapter.now=()=>now;
 const wait=join(attemptDir(h.home,h.integrated.integratedCommit,0),"host-wait.json");
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.matrix().length,0);assert.equal(h.sends().length,0);
 assert.deepEqual(JSON.parse(readFileSync(wait,"utf8")),{since:1000});assert.equal(statSync(wait).mode&0o777,0o600);
 now+=MATRIX_HOST_WAIT_MS-1;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.matrix().length,0);
 busy=0;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.deepEqual(h.matrix().map(c=>c.argv[2]),["plan","run"]);assert.equal(h.sends().length,1);assert.ok(!existsSync(wait));
 const late=matrixHost();late.adapter.matrixRunsActive=()=>1;let t=0;late.adapter.now=()=>t;
 assert.equal(await late.adapter.verifyIntegrated(late.integrated),false);t=MATRIX_HOST_WAIT_MS;
 await assert.rejects(late.adapter.verifyIntegrated(late.integrated),e=>failureReason(e)==="Host busy with another verify-matrix run");assert.equal(late.matrix().length,0);
 const short=matrixHost();short.adapter.config.matrixHostWaitMs=50;short.adapter.matrixRunsActive=()=>1;let s=0;short.adapter.now=()=>s;
 assert.equal(await short.adapter.verifyIntegrated(short.integrated),false);s=50;await assert.rejects(short.adapter.verifyIntegrated(short.integrated),/Host busy/);
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
test("p2 a requeued job starts a fresh host wait, and a busy refusal leaves no wait record",async()=>{
 const h=matrixHost();let now=10*MATRIX_HOST_WAIT_MS;h.adapter.now=()=>now;h.adapter.matrixRunsActive=()=>1;
 const first=attemptDir(h.home,h.integrated.integratedCommit,0),requeued={...h.integrated,reconciliations:[{disposition:"requeue"}]};
 mkdirSync(first,{recursive:true});writeFileSync(join(first,"host-wait.json"),JSON.stringify({since:now-3*3600000}));
 assert.equal(await h.adapter.verifyIntegrated(requeued),false,"a leftover start does not refuse the requeued job's first busy probe");
 assert.deepEqual(JSON.parse(readFileSync(join(attemptDir(h.home,h.integrated.integratedCommit,1),"host-wait.json"),"utf8")),{since:now});
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),e=>failureReason(e)==="Host busy with another verify-matrix run");
 assert.ok(!existsSync(join(first,"host-wait.json")),"the refusal removes its wait record");
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false,"a later probe starts a fresh wait");assert.equal(h.matrix().length,0);
});
test("p2 a requeue never reuses an earlier attempt's receipt, for the same or a new integrated commit",async()=>{
 const receipt={environment:{},checks:[{exitCode:1}]},h=matrixHost({receipt}),old=h.integrated.integratedCommit,moved="d".repeat(40);
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),e=>failureReason(e)==="Integrated matrix receipt is not eligible");
 await assert.rejects(h.adapter.verifyIntegrated(h.integrated),/not eligible/);assert.equal(h.matrix().filter(c=>c.argv[2]==="run").length,1,"the same attempt does not rerun");
 receipt.checks=[{exitCode:0}];h.calls.length=0;
 assert.equal(await h.adapter.verifyIntegrated({...h.integrated,reconciliations:[{}]}),false);
 assert.deepEqual(h.matrix().map(c=>c.argv[2]),["plan","run"],"a requeue of the same commit reruns");
 assert.ok(h.sends()[0].argv.includes(join(attemptDir(h.home,old,1),"receipt.json")));
 h.calls.length=0;
 assert.equal(await h.adapter.verifyIntegrated({...h.integrated,integratedCommit:moved,reconciliations:[{},{}]}),false);
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
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
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
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
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
 const host=new HostAdapter({cwd:f.cwd,journalDirectory:home},{id:"rel_fixture",agentId:"agt_fixture",runId:"run_fixture",generation:1});host.matrixRunsActive=()=>0;
 host.command=argv=>{argvs.push(argv);
  if(argv[1]==="deployment"&&argv[2]==="list")return JSON.stringify([{id:"rel_fixture",state:"claimed",matrixApprovals:approvals}]);
  if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123abcd"});
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){writeFileSync(argv[4],JSON.stringify(workedPlan));return "";}
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="run"){writeFileSync(join(argv[4],"receipt.json"),JSON.stringify({environment:{},checks:[{exitCode:0}]}));return "";}
  return "";};
 a.verifyIntegrated=x=>host.verifyIntegrated(x);a.escalate=async d=>{a.calls.push("escalate");await host.escalate(d);};
 return {f,j,c,a,argvs,home,tip,originHead,sends:()=>argvs.filter(x=>x[1]==="send"),matrix:()=>argvs.filter(x=>x[1]==="scripts/verify-matrix.mjs")};
}
test("m4 end to end: a matrix change on tasks-hub after acceptance verifies under the owner approval of the new digest",async()=>{
 const r=matrixChangeRelease([{digest:hash(MATRIX_A),messageSeq:11},{digest:hash(MATRIX_B),messageSeq:40}]);
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
