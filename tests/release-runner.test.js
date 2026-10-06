import test from "node:test";
import {createServer} from "node:http";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,copyFileSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,readdirSync,rmSync,symlinkSync,renameSync,realpathSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname,resolve} from "node:path";
import {execFileSync,execFile,spawn,spawnSync} from "node:child_process";
import {integrateCandidate,publishIntegration,runRelease,liveCheck,runnableJob,HostAdapter,serveDeployment,hostLockNames,retentionPolicy,pruneJournal,reconcileHostLocks,revertCommit,moveReleaseRef,tasksHubCheckedOut,releaseError,compatibilityArgv,dispatchCompatibility,validateNativeRelease,failureReason,failureDetail,MATRIX_PREREQUISITES,MATRIX_HOST_WAIT_MS,MATRIX_LAUNCH_GRACE_MS,MATRIX_STOP_GRACE_MS,MATRIX_DEADLINE_SLACK_MS,missingPrerequisites,provisionPrerequisites,matrixRunTimeout,matrixPriority,matrixWaitNotice,matrixHeldNotice,fenceWaitNotice,matrixRunUnsettled,RUNNER_CODE_FILES,LOADED_CODE,CODE_REASONS,codeDigest,diskCode,publishedCode,codeDecision,prepareCode,runnerCodeGate,codeRecord,codeNotice,cliFailureNotice} from "../scripts/release-runner.mjs";
import {acquireHostLock,readHostState as rawReadHostState,holdersOf,readJournal,updateHostState,pidGone,groupGone,RUN_TIMEOUT_GRACE_MS,DEFAULT_HOLDER_CAP_MS} from "../scripts/verify-matrix-host-lock.mjs";
import {planRunTimeout,readPrerequisites} from "../scripts/verify-matrix.mjs";
import {createHash} from "node:crypto";
import {renderTeamDelivery} from "../client/team-delivery-view.js";
import {targetsForPaths,selectReleaseTargets} from "../scripts/release-targets.mjs";
function releaseReply(jobs,argv) {
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
const releaseReplySource = "const releaseReply="+releaseReply.toString()+";\n";
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
 writeFileSync(tt,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const args=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},args.join(' ')+'\\n');if(args[0]!=='deployment'||args[1]!=='list')process.exit(99);`+body);chmodSync(tt,0o755);
 writeFileSync(configPath,JSON.stringify({version:1,enabled:true,cwd,journalDirectory:cwd,tt,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(x=>[x,"a".repeat(40)]))}));
 const poll=()=>spawnSync(process.execPath,[RUNNER,"--config",configPath,"--once"],{cwd,encoding:"utf8",stdio:["ignore","pipe","pipe"],timeout:30000});
 return {cwd,tt,log,poll};
}
test("buf1 a valid fake tt deployment list above eight MiB parses and polls",t=>{
 const f=bufferTT(t,`fs.writeSync(1,JSON.stringify(releaseReply([{id:'rel_fixture',state:'refused',padding:'x'.repeat(9*1024*1024)}],args)));`);
 const adapter=new HostAdapter({cwd:f.cwd,tt:f.tt},{}),raw=adapter.command([f.tt,"deployment","list","--view","settled"]);
 assert.ok(Buffer.byteLength(raw)>8*1024*1024);const jobs=JSON.parse(raw);
 assert.equal(jobs.version,1);assert.equal(jobs.jobs.length,1);assert.equal(jobs.jobs[0].padding.length,9*1024*1024);
 const poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");assert.equal(poll.stderr,"");
 assert.equal(readFileSync(f.log,"utf8"),"deployment list --view settled\ndeployment list --view active --limit 200\ndeployment list --view settled --limit 200 --snapshot "+"a".repeat(64)+"\ndeployment list --view active --limit 200 --snapshot "+"a".repeat(64)+"\n");
});
test("buf2 real fake tt command overflow names ENOBUFS without payloads or claims",t=>{
 const f=bufferTT(t,`fs.writeSync(2,'SYNTHETIC_PRIVATE_TOKEN');const chunk=Buffer.alloc(1024*1024,120);for(let i=0;i<257;i++)fs.writeSync(2,chunk);`);
 const adapter=new HostAdapter({cwd:f.cwd},{});
 assert.throws(()=>adapter.command([f.tt,"deployment","list"]),e=>e.message==="Host operation failed" && failureReason(e)==="tt ENOBUFS (command buffer overflow)");
 const poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");const diagnostic=poll.stderr;
 assert.match(diagnostic,/Deployment poll held.*ENOBUFS.*command buffer overflow/);assert.ok(diagnostic.length<256);assert.ok(!diagnostic.includes("SYNTHETIC_PRIVATE_TOKEN"));
 const calls=readFileSync(f.log,"utf8").split("\n");assert.deepEqual(calls.slice(0,2),["deployment list","deployment list --view active --limit 200"]);
 assert.match(calls[2],/^send --kind notice --subject A deployer CLI call failed --text Deployer CLI call failed: tt deployment list ENOBUFS \(command buffer overflow\)\. /);assert.ok(!calls[2].includes("SYNTHETIC_PRIVATE_TOKEN"));assert.deepEqual(calls.slice(3),[""]);
});
test("buf3 fake tt errors and invalid JSON keep bounded non-secret poll diagnostics",t=>{
 for(const body of [`fs.writeSync(1,'SYNTHETIC_PRIVATE_TOKEN');fs.writeSync(2,'SYNTHETIC_PRIVATE_TOKEN');process.exit(2);`,`fs.writeSync(1,'SYNTHETIC_PRIVATE_TOKEN');`]){
  const f=bufferTT(t,body),poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");
  const diagnostic=poll.stderr;assert.match(diagnostic,/Deployment poll held/);assert.ok(diagnostic.length<256);assert.ok(!diagnostic.includes("SYNTHETIC_PRIVATE_TOKEN"));
  assert.match(diagnostic,body.includes("process.exit")?/tt deployment list exit 2/:/unclassified/);const calls=readFileSync(f.log,"utf8").split("\n");assert.equal(calls[0],"deployment list --view active --limit 200");
  if(body.includes("process.exit")){assert.match(calls[1],/^send --kind notice --subject A deployer CLI call failed --text Deployer CLI call failed: tt deployment list exit 2\. /);assert.ok(!calls[1].includes("SYNTHETIC_PRIVATE_TOKEN"));assert.deepEqual(calls.slice(2),[""]);}
  else assert.deepEqual(calls.slice(1),[""]);
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
 const cwd=mkdtempSync(join(tmpdir(),"receipt-adapter-")),adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture",state:"merged",generation:5});let args;
 adapter.command=argv=>{args=argv;return JSON.stringify({...adapter.job,generation:6,state:"released",receipt});};
 const receipt={version:1,jobId:"rel_fixture",outcome:"released",targets:[]};await adapter.finish(receipt,5);
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
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify(jobs)},a)));else process.exit(2);`);chmodSync(fakeTT,0o755);
 const base={version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT};
 for(const retention of [{releasedBackups:-1},{releasedBackups:1.5},{releasedBackups:"3"},{backupBudgetBytes:-1},{backupBudgetBytes:0.5},{backupBudgetBytes:null},"3",[3]])await assert.rejects(serveDeployment({...base,retention},{once:true}),/Invalid journal retention/);
 assert.ok(!existsSync(log),"no command ran");
 // The default policy keeps three released copies: the poll removes the oldest and still tries the waiting job.
 for(const x of jobs.slice(0,4))writeFileSync(join(cwd,copyOf(x.id)),"copy");
 await serveDeployment(base,{once:true});assert.ok(!existsSync(join(cwd,copyOf("rel_1"))));assert.deepEqual(["rel_2","rel_3","rel_4"].filter(id=>existsSync(join(cwd,copyOf(id)))),["rel_2","rel_3","rel_4"]);
 assert.match(readFileSync(log,"utf8"),/deployment claim --job next /);
 // A removal record that cannot be written fails the sweep: the copy stays and the poll goes on to the claim.
 rmSync(log);rmSync(join(cwd,"retention.jsonl"));mkdirSync(join(cwd,"retention.jsonl"));
 await serveDeployment({...base,retention:{releasedBackups:0}},{once:true});assert.ok(existsSync(join(cwd,copyOf("rel_4"))));assert.match(readFileSync(log,"utf8"),/deployment claim --job next /);
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
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(['list','get'].includes(a[1])){console.log(JSON.stringify(releaseReply([{id:'stale',state:'verified',generation:1},{id:'later',state:'verified',generation:1}],a)));}else{process.exit(2);}`);chmodSync(fakeTT,0o755);
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
 adapter.command=argv=>{calls.push(argv);if(argv[1]==="deployment"&&argv[2]==="handler")return JSON.stringify({id:"agt_0123456789abcdef",name:"db-handler-sol61"});if(argv[2]==="get")return JSON.stringify({...adapter.job,state:"claimed"});return "";};
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
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply([${JSON.stringify(superseded)}],a)));else process.exit(2);`);chmodSync(fakeTT,0o755);
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT},{once:true});assert.equal(readFileSync(log,"utf8"),"deployment list --view active --limit 200\ndeployment list --view settled --limit 200 --snapshot "+"a".repeat(64)+"\ndeployment list --view active --limit 200 --snapshot "+"a".repeat(64)+"\n");
});
test("b1 a recorded hand release advances the next poll's baselines with no config edit",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});const tt=change(f,"hub/cmd/tt/main.go","cli"),commit=change(f,"client/a.js","a");
 const home=mkdtempSync(join(tmpdir(),"release-hand-baselines-")),configPath=join(home,"deploy.json"),fakeTT=join(home,"tt");
 const all=b=>Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,b]));
 writeFileSync(configPath,JSON.stringify({version:1,enabled:true,cwd:f.cwd,journalDirectory:home,tt:fakeTT,baselines:all(f.base)}));
 const superseded={id:"rel_hand",state:"superseded",settledAt:"2026-09-30T12:00:00.5Z",supersession:{releasedCommit:tt,release:"20260930-hand",handReleaseId:"hrl_0123456789abcdef",targets:["mini"]}};
 const verified={id:"rel_next",state:"verified",generation:1,commit};
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const a=process.argv.slice(2);if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify([superseded,verified])},a)));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...verified,state:"claimed",generation:2,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN})}));else process.exit(2);`);chmodSync(fakeTT,0o755);
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
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const a=process.argv.slice(2);if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply([${JSON.stringify(verified)}],a)));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...verified,state:"claimed",generation:2,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN})}));else process.exit(2);`);chmodSync(fakeTT,0o755);
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
 const h=fixture(),m=job(h,change(h,"client/a.js","a")),e=fake(),n=config(h,m);let fences=0;e.fence=async()=>++fences<4;
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
  if(argv[1]==="deployment"&&argv[2]==="get")return JSON.stringify({...adapter.job,state:"claimed",generation:adapter.job.generation??1,...(jobs.find(j=>j.id==="rel_fixture")||{})});
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
 const host=new HostAdapter({cwd:f.cwd,journalDirectory:dirname(c.journalPath)},{id:"rel_fixture"});host.command=argv=>{argvs.push(argv);return argv[2]==="get"?JSON.stringify({...host.job,state:"claimed",generation:host.job.generation??1}):"";};
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
 assert.equal(matrixRunTimeout({checks:[{environment:{VERIFICATION_TIMEOUT_MS:"3600000"}}]}),5400000,"the one-hour check ceiling is admitted");
 assert.equal(matrixRunTimeout({maxAttempts:3,checks:[{environment:{VERIFICATION_TIMEOUT_MS:"3000000"}},{environment:{VERIFICATION_TIMEOUT_MS:"240000"}}]}),7200000,"the prescribed limits still clamp to the unchanged holder cap");
 for(const bad of [{...workedPlan,checks:[]},{checks:[{environment:{}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:"3600001"}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:"0"}}]},{checks:[{environment:{VERIFICATION_TIMEOUT_MS:1800000}}]},{maxAttempts:0,checks:workedPlan.checks}])
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
 const bad=matrixHost({plan:{maxAttempts:3,checks:[{environment:{VERIFICATION_TIMEOUT_MS:"3600001"}}]}});
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
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const a=process.argv.slice(2);if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify([verified])},a)));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...verified,state:"claimed",generation:2,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN})}));else if(a[0]==='send')require('fs').appendFileSync(${JSON.stringify(log)},JSON.stringify(a)+'\\n');else process.exit(2);`);chmodSync(fakeTT,0o755);
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
  writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const a=process.argv.slice(2);if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify([a,b])},a)));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...a,state:"claimed",generation:2,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN})}));else if(a[0]==='send')require('fs').appendFileSync(${JSON.stringify(log)},JSON.stringify(a)+'\\n');else process.exit(2);`);chmodSync(fakeTT,0o755);
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
 writeFileSync(fakeTT,`#!/bin/sh\necho "$@" >> ${JSON.stringify(log)}\necho '{"version":1,"jobs":[],"page":{"view":"'$4'","limit":200,"snapshot":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","nextAfter":""}}'\n`);chmodSync(fakeTT,0o755);
 for(const bad of [-1,300001,"90000",1.5])await assert.rejects(serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{tailos:{switchWindowMs:bad}}},{once:true}),/switch window/);
 assert.equal(existsSync(log),false);
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{tailos:{switchWindowMs:120000}}},{once:true});assert.equal(readFileSync(log,"utf8"),"deployment list --view active --limit 200\ndeployment list --view settled --limit 200 --snapshot "+"a".repeat(64)+"\ndeployment list --view active --limit 200 --snapshot "+"a".repeat(64)+"\n");
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
  if(argv[1]==="deployment"&&argv[2]==="get")return JSON.stringify({...host.job,state:"claimed",matrixApprovals:approvals});
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
 writeFileSync(fakeTT,`#!/bin/sh\necho "$@" >> ${JSON.stringify(log)}\necho '{"version":1,"jobs":[],"page":{"view":"'$4'","limit":200,"snapshot":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","nextAfter":""}}'\n`);chmodSync(fakeTT,0o755);
 for(const target of ["hub","bridge"])for(const bad of [-1,300001,"240000",1.5])await assert.rejects(serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{[target]:{host:"truenas",readyWindowMs:bad}}},{once:true}),/readiness window/);
 assert.equal(existsSync(log),false);
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT,targets:{hub:{host:"truenas",readyWindowMs:120000},bridge:{host:"truenas"}}},{once:true});assert.equal(readFileSync(log,"utf8"),"deployment list --view active --limit 200\ndeployment list --view settled --limit 200 --snapshot "+"a".repeat(64)+"\ndeployment list --view active --limit 200 --snapshot "+"a".repeat(64)+"\n");
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
 const before=await journalOf(...start(),a=>{let checks=0;a.fence=async()=>{if(!checks++)return true;throw releaseError(shaped);};});
 assert.equal(before.journal.refusalReason,"unclassified");assert.equal(before.journal.failure,undefined);assert.deepEqual(before.journal.failureDetail,{tagRejected:"characters",tagLength:shaped.length});
 absent(before.raw+JSON.stringify(before.escalation),TOKEN,"token=");
 const long="SYNTHETICPRIVATETOKEN".repeat(10),rc=await journalOf(...start(),a=>{a.prepare=async()=>{throw releaseError(long);};},/Release failed/);
 assert.deepEqual(rc.journal.failureDetail,{tagRejected:"length",tagLength:210});absent(rc.raw,"SYNTHETICPRIVATETOKEN");
 // An attempt record's refusal is read back from disk, so it is validated too.
 const d=fixture(),dj=job(d,change(d,"client/a.js","a"));git(d.cwd,"checkout","tasks-hub");change(d,"client/c.js","c");
 const rd=await journalOf(d,dj,(a,c)=>{
  const host=new HostAdapter({cwd:d.cwd,journalDirectory:dirname(c.journalPath)},{id:"rel_fixture"});host.command=argv=>argv[2]==="get"?JSON.stringify({...host.job,state:"claimed",generation:host.job.generation??1}):"";
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

test("bounded poll refuses incomplete history before claims, receipt writes or pruning",async t=>{
 for(const fault of ["legacy","view","snapshot","repeat","duplicate","empty","date"]){
  const f=bufferTT(t,`const view=args[args.indexOf('--view')+1];let p=releaseReply([{id:'rel_active',state:'verified',generation:1}],args);if(view==='settled'){if(${JSON.stringify(fault)}==='legacy')p=[];else if(${JSON.stringify(fault)}==='view')p.page.view='active';else if(${JSON.stringify(fault)}==='snapshot')p.page.snapshot='b'.repeat(64);else if(${JSON.stringify(fault)}==='duplicate')p.jobs=[{id:'rel_active',summary:true,rowId:1,state:'released'}];else if(${JSON.stringify(fault)}==='date')p.jobs=[{id:'rel_history',summary:true,rowId:2,state:'released',generation:1,settledAt:'invalid',receipt:{outcome:'released',commit:'c'.repeat(40),targets:[]}}];else {p.page.nextAfter='same';if(${JSON.stringify(fault)}==='repeat')p.jobs=[{id:'rel_history',summary:true,rowId:2,state:'released'}];}}console.log(JSON.stringify(p));`);
  const copy=join(f.cwd,"rel_history-truenas-backup.sqlite"),pending=join(f.cwd,"rel_history.json");writeFileSync(copy,"keep");writeFileSync(pending,JSON.stringify({jobId:"rel_history",phase:"receipt_pending",receipt:{pin:"keep"}}));
  const before=readFileSync(pending,"utf8"),poll=f.poll();assert.equal(poll.status,0);assert.match(poll.stderr,/poll held/);
  const commands=readFileSync(f.log,"utf8").trim().split("\n");assert.ok(commands.every(x=>x.startsWith("deployment list ")));
  assert.equal(readFileSync(copy,"utf8"),"keep");assert.equal(readFileSync(pending,"utf8"),before);assert.ok(!existsSync(join(f.cwd,"retention.jsonl")));
 }
});

test("lost finish uses exactly one full detail and holds mismatched artifact pins",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-paged-receipt-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const log=join(cwd,"calls"),tt=join(cwd,"tt"),journal=join(cwd,"rel_done.json");
 const receipt={version:1,jobId:"rel_done",outcome:"released",commit:"c".repeat(40),verificationDigest:"d".repeat(64),targets:[{target:"hub",outcome:"released",release:"exact",artifactSHA256:"e".repeat(64),backup:"exact-backup",backupSHA256:"f".repeat(64)}]};
 const pending={jobId:"rel_done",commit:receipt.commit,finishGeneration:3,phase:"receipt_pending",receipt};
 const poll=async changed=>{
  const detail={id:"rel_done",state:"released",generation:4,commit:receipt.commit,integratedCommit:receipt.commit,verificationDigest:receipt.verificationDigest,receipt:structuredClone(receipt)};
  if(changed)detail.receipt.targets[0].artifactSHA256="0".repeat(64);
  writeFileSync(tt,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify([detail])},a)));else process.exit(99);`);chmodSync(tt,0o755);
  writeFileSync(journal,JSON.stringify(pending));await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt},{once:true});
 };
 await poll(true);assert.equal(JSON.parse(readFileSync(journal,"utf8")).phase,"receipt_pending");assert.ok(!existsSync(join(cwd,"retention.jsonl")));
 await poll(false);assert.equal(JSON.parse(readFileSync(journal,"utf8")).phase,"complete");
 // Completed historical journals do not cause a full-detail read on every poll.
 await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt},{once:true});
 const reads=readFileSync(log,"utf8").trim().split("\n");assert.equal(reads.filter(x=>x==="deployment get --job rel_done").length,2);assert.ok(reads.every(x=>x.startsWith("deployment list ")||x.startsWith("deployment get ")));
});

test("fake CLI traverses more than two hundred active jobs and five history pages before one claim",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-many-pages-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const tt=join(cwd,"tt"),log=join(cwd,"calls"),targets=["hub","bridge","mini","tailos"],baseline="b".repeat(40);
 const jobs=[...Array.from({length:205},(_,i)=>({id:`rel_active_${i}`,state:"verified",generation:1,commit:"c".repeat(40)})),...Array.from({length:805},(_,i)=>({id:`rel_settled_${i}`,state:"released",generation:2,receipt:{outcome:"released",commit:baseline,targets:targets.map(target=>({target,outcome:"released",artifactSHA256:"d".repeat(64)}))}}))];
 const settled=(i,state,at,receipt,supersession)=>Object.assign(jobs[205+i],{state,settledAt:at,receipt,supersession});
 const shipped=(commit,targets)=>({outcome:"released",commit,targets:targets.map(target=>({target,outcome:"released"}))});
 settled(799,"rolled_back","2026-10-04T10:00:00.000000099Z",{outcome:"rolled_back",commit:"f".repeat(40),targets:targets.map(target=>({target,outcome:"rolled_back"}))});
 settled(800,"released","2026-10-04T10:00:00.000000009Z",shipped("c".repeat(40),["hub"]));
 settled(801,"released","2026-10-04T10:00:00.000000001Z",shipped(baseline,["hub"]));
 settled(802,"superseded","2026-10-04T10:00:00.000000009Z",undefined,{releasedCommit:"d".repeat(40),targets:["hub","mini"]});
 settled(803,"released","2026-10-04T10:00:00.000000009Z",shipped("e".repeat(40),["bridge","tailos"]));
 settled(804,"refused","2026-10-04T10:00:00.000000099Z",undefined);
 const expected={hub:"d".repeat(40),bridge:"e".repeat(40),mini:"d".repeat(40),tailos:"e".repeat(40)};
 writeFileSync(tt,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const a=process.argv.slice(2),jobs=${JSON.stringify(jobs)};fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(jobs,a)));else if(a[1]==='claim')console.log(JSON.stringify({...jobs[0],state:'claimed',generation:2,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN}));else process.exit(99);`);chmodSync(tt,0o755);
 let releases=0;await serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt,baselines:Object.fromEntries(targets.map(t=>[t,"a".repeat(40)]))},{once:true,release:async config=>{releases++;assert.equal(config.job.id,jobs[0].id);assert.deepEqual(config.baselines,expected);return {outcome:"released"};}});
 assert.equal(releases,1);const calls=readFileSync(log,"utf8").trim().split("\n");
 assert.equal(calls.filter(x=>x.includes("--view active")).length,4);assert.equal(calls.filter(x=>x.includes("--view settled")).length,5);
 assert.equal(calls.filter(x=>x.startsWith("deployment get")).length,1);assert.equal(calls.filter(x=>x.startsWith("deployment claim")).length,1);
 assert.ok(calls.slice(0,7).every(x=>x.startsWith("deployment list ")));assert.equal(calls[7],"deployment get --job rel_active_0");
});

test("dedicated dispatcher rewrites only deployment reads and refuses recursion", t => {
 const dir=mkdtempSync(join(tmpdir(),"release-dispatch-"));t.after(()=>rmSync(dir,{recursive:true,force:true}));
 const binary=join(dir,"stable-tt");writeFileSync(binary,"#!/bin/sh\nexit 0\n");chmodSync(binary,0o755);
 for(const argv of [["deployment","list"],["deployment","list","--view","settled","--limit","200"],["deployment","get","--job","rel_fixture"],["deployment","finish","--file","literal;$(echo no)"],["send","--text","literal;$(echo no)"]]) {
  let called;const code=dispatchCompatibility(binary,argv,{run:(path,args,options)=>{called={path,args,options};return {status:7};}});
  assert.equal(code,7);assert.deepEqual(called.args,compatibilityArgv(argv));assert.equal(called.path,realpathSync(binary));assert.equal(called.options.stdio,"inherit");
 }
 assert.deepEqual(compatibilityArgv(["deployment","finish","--job","x"]),["deployment","finish","--job","x"]);
 assert.throws(()=>dispatchCompatibility(RUNNER,["deployment","list"]));
 assert.throws(()=>dispatchCompatibility(process.execPath,["deployment","list"]),/Recursive/);
 assert.throws(()=>dispatchCompatibility("tt",["deployment","list"]),/Absolute/);
 assert.throws(()=>compatibilityArgv(["deployment","list","line\nbreak"]),/Invalid/);
});

test("native response faults never replace the exact execution job or touch release files", async t => {
 const f=fixture(),commit=change(f,"client/a.js","a"),home=mkdtempSync(join(tmpdir(),"release-native-pins-"));
 t.after(()=>rmSync(home,{recursive:true,force:true}));
 const prior={...job(f,commit),taskId:"tsk_fixture",entryId:"tqe_fixture",itemId:"wi_fixture",itemRevision:5,scopeRevision:5,orderMessageSeq:23666,repository:f.cwd,generation:2,agentId:"agt_fixture",runId:"run_fixture",pauseGeneration:0,integratedCommit:commit,inputsCommit:commit,inputsDigest:"b".repeat(64)};
 const faultKeys=["id","taskId","entryId","itemId","itemRevision","scopeRevision","orderMessageSeq","repository","baseCommit","commit","verificationDigest","pauseGeneration","generation","agentId","runId","integratedCommit","inputsCommit","inputsDigest","state","summary"];
 for(const key of faultKeys) {
  const bad={...prior,[key]:typeof prior[key]==="number"?prior[key]+1:key==="summary"?true:"other"};
  const host=new HostAdapter({cwd:f.cwd,journalDirectory:home},prior);host.command=()=>JSON.stringify(bad);
  assert.equal(await host.fence(),false,key);assert.equal(host.job,prior,key);
  const before=git(f.cwd,"rev-parse","tasks-hub");
  await assert.rejects(runRelease({...config(f,prior),journalPath:join(home,"journal.json")},host),/fence lost/);
  assert.deepEqual(readdirSync(home),[],key);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),before,key);
 }
 assert.equal(validateNativeRelease(prior,{...prior},"check",2).generation,2);
 const receipt={version:1,jobId:prior.id,commit,verificationDigest:prior.verificationDigest,outcome:"released",targets:[{target:"hub",outcome:"released",release:"exact",artifactSHA256:"c".repeat(64),backup:"retained",backupSHA256:"d".repeat(64),preflightReceiptSHA256:"e".repeat(64)}]};
 assert.throws(()=>validateNativeRelease({...prior,state:"merged"},{...prior,state:"released",generation:3,receipt:{outcome:"released",commit,targets:[]}},"finish",2,[],receipt),/receipt/);
 assert.equal(validateNativeRelease({...prior,state:"merged"},{...prior,state:"released",generation:3,receipt},"finish",2,[],receipt).receipt,receipt);
});

test("detail projection and snapshot faults hold inputs before probes, plans, backups or manifests", async t => {
 const {buildInputs,readReleaseSummaries,deploymentBaselines}=await import("../scripts/release-inputs.mjs");
 const f=fixture(),commit=change(f,"client/a.js","a"),home=mkdtempSync(join(tmpdir(),"release-input-fault-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const task="tsk_fixture",j={...job(f,commit),id:"rel_0123abcd",taskId:task,generation:2,agentId:"agt_fixture",runId:"run_fixture",pauseGeneration:0,integratedCommit:commit};
 const config={version:1,cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base]))};
 for(const fault of ["taskId","agentId","runId","pauseGeneration","commit","baseCommit","verificationDigest","integratedCommit","inputsCommit","inputsDigest","generation","bookend","compact"]) {
  const calls=[];let detail=false;
  const read=argv=>{
   calls.push(argv);if(argv[1]==="get") {detail=true;return JSON.stringify(fault==="compact"?{...j,summary:true}:{...j,...(!["bookend","compact"].includes(fault)?{[fault]:typeof j[fault]==="number"?j[fault]+1:"changed"}:{})});}
   const page=releaseReply([j],argv);if(fault==="bookend" && detail)page.page.snapshot="b".repeat(64);return JSON.stringify(page);
  };
  const deps={tt:read,git:argv=>git(f.cwd,...argv),probe:()=>assert.fail("probe"),preflight:()=>assert.fail("backup"),copyBackup:()=>assert.fail("copy")};
  await assert.rejects(buildInputs(config,j.id,{deps}),/detail|ledger/i,fault);assert.deepEqual(readdirSync(home),[]);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);
 }
 // Exact fractional (nanosecond) ordering and equal-time ledger ties.
 const targets=["hub","bridge","mini","tailos"],released=(id,commit,at)=>({id,state:"released",settledAt:at,receipt:{outcome:"released",commit,targets:targets.map(target=>({target,outcome:"released"}))}});
 const rows=[released("late","c".repeat(40),"2026-10-04T10:00:00.000000009Z"),released("early","b".repeat(40),"2026-10-04T10:00:00.000000001Z")];
 assert.deepEqual(deploymentBaselines(config.baselines,rows),Object.fromEntries(targets.map(t=>[t,"c".repeat(40)])));
 rows.push(released("tie","d".repeat(40),"2026-10-04T10:00:00.000000009Z"));
 assert.deepEqual(deploymentBaselines(config.baselines,rows),Object.fromEntries(targets.map(t=>[t,"d".repeat(40)])));
 const mixed=[j,{...j,id:"rel_2",taskId:"tsk_other"}];assert.throws(()=>readReleaseSummaries(a=>JSON.stringify(releaseReply(mixed,a))),/task changed/);
});

test("immutable flat and paged consumers cross both APIs with a stable binary through Mini rollback", {timeout:180000}, async t => {
 const root=dirname(dirname(RUNNER)),dir=mkdtempSync(join(tmpdir(),"release-wire-crossing-")),historical=join(dir,"ab9"),stable=join(dir,"compat-tt"),installed=join(dir,"installed-tt");
 mkdirSync(historical);t.after(()=>rmSync(dir,{recursive:true,force:true}));
 const archive=execFileSync("git",["archive","ab9c4382d5f585c5d69f1d150d524d5670790b32","hub","scripts","verification","package.json","tests/test-binaries.mjs"],{cwd:root,maxBuffer:64*1024*1024});
 execFileSync("tar",["-xf","-","-C",historical],{input:archive});
 execFileSync("go",["build","-o",stable,"./cmd/tt"],{cwd:join(root,"hub"),timeout:90000,stdio:"pipe"});
 const old=join(dir,"old-tt");execFileSync("go",["build","-o",old,"./cmd/tt"],{cwd:join(historical,"hub"),timeout:90000,stdio:"pipe"});
 const shim=join(dir,"shim.mjs");writeFileSync(shim,"#!"+process.execPath+"\nimport {dispatchCompatibility} from "+JSON.stringify("file://"+RUNNER)+";process.exitCode=dispatchCompatibility("+JSON.stringify(stable)+",process.argv.slice(2));\n");chmodSync(shim,0o755);
 const f=fixture(),commit=change(f,"client/a.js","a"),task="tsk_0123456789abcdef",jobID="rel_0123456789abcdef";
 let mode="legacy",failure=0;const methods=[];
 const jobs=[{id:jobID,taskId:task,state:"claimed",generation:2,commit,baseCommit:f.base,verificationDigest:"d".repeat(64),plan:{commit},agentId:"agt_other",runId:"run_other",pauseGeneration:0},
 {id:"rel_1111111111111111",taskId:task,state:"superseded",generation:2,plan:{commit:f.base},settledAt:"2026-10-04T10:00:00.000000001Z",supersession:{releasedCommit:f.base,targets:["hub","bridge","mini","tailos"]}}];
 const server=createServer((req,res)=>{
  methods.push(req.method+" "+new URL(req.url,"http://fixture").pathname);
  if(req.method!=="GET" || req.headers.authorization!=="Bearer synthetic-fixture" || failure){res.writeHead(failure||403);res.end("{}");return;}
  const q=new URL(req.url,"http://fixture").searchParams;
  if(mode==="legacy"){res.end(JSON.stringify(jobs));return;}
  const token=hash(JSON.stringify(jobs));
  if(q.get("snapshot") && q.get("snapshot")!==token){res.writeHead(409);res.end("{}");return;}
  if(q.get("job")){res.end(JSON.stringify(jobs.find(j=>j.id===q.get("job"))));return;}
  const argv=["deployment","list",...Array.from(q).flatMap(([k,v])=>["--"+k,v])],p=releaseReply(jobs,argv);p.page.snapshot=token;res.end(JSON.stringify(p));
 });
 await new Promise(r=>server.listen(0,"127.0.0.1",r));t.after(()=>new Promise(r=>server.close(r)));
 const environment={...process.env,HOME:dir,TAILTERM_HUB:`http://127.0.0.1:${server.address().port}`,TAILTERM_TASK:task,TAILTERM_TOKEN:"synthetic-fixture",TAILTERM_AGENT:"",TAILTERM_RUN:"",TAILTERM_AGENT_NAME:"",CODEX_THREAD_ID:"",TAILTERM_CODEX_THREAD:"",TT_TMUX_SOCKET:"compat-isolated-"+process.pid};
 const run=(binary,argv)=>new Promise(resolve=>execFile(binary,argv,{cwd:f.cwd,env:environment,timeout:30000,maxBuffer:32*1024*1024},(err,stdout,stderr)=>resolve({err,stdout,stderr})));
 // Same stable reader handles hub-first, Mini-first, then both reverse orders.
 for(const [api,mini] of [["legacy",old],["modern",old],["modern",stable],["legacy",stable],["legacy",old],["modern",stable],["modern",old],["legacy",old]]){
  mode=api;copyFileSync(mini,installed);
  const flat=await run(shim,["deployment","list"]);assert.equal(flat.err,null,flat.stderr);const full=JSON.parse(flat.stdout);assert.equal(full[0].plan.commit,commit);assert.equal(full.length,2);
  const paged=await run(shim,["deployment","list","--view","active","--limit","200"]);assert.equal(paged.err,null,paged.stderr);assert.equal(JSON.parse(paged.stdout).jobs[0].summary,true);
  const detail=await run(shim,["deployment","get","--job",jobID]);assert.equal(detail.err,null,detail.stderr);assert.equal(JSON.parse(detail.stdout).verificationDigest,jobs[0].verificationDigest);
 }
 // Immutable incompatible pairs fail, and produce no partial output.
 mode="modern";let bad=await run(old,["deployment","list"]);assert(bad.err);assert.equal(bad.stdout,"");
 mode="legacy";bad=await run(stable,["deployment","list"]);assert(bad.err);assert.equal(bad.stdout,"");
 // Execute the actual immutable flat and current paged input preparers.
 const configPath=join(dir,"config.json");writeFileSync(configPath,JSON.stringify({version:1,enabled:true,cwd:f.cwd,journalDirectory:dir,tt:shim,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base]))}));
 for(const api of ["legacy","modern"]){mode=api;for(const source of [historical,root]){
  const result=await run(process.execPath,[realpathSync(join(source,"scripts/release-inputs.mjs")),"--config",configPath,"--job",jobID,"--dry-run"]);
  assert.equal(result.err,null,result.stderr);const planned=JSON.parse(result.stdout);assert.equal(planned.commit,commit);assert.deepEqual(Object.keys(planned.targets),["tailos"]);
  // The immutable old daemon consumes flat output; the new one consumes
  // paged output. This other run holds the fence and neither may act on it.
  const poll=await run(process.execPath,[realpathSync(join(source,"scripts/release-runner.mjs")),"--config",configPath,"--once"]);
  assert.equal(poll.err,null,poll.stderr);assert.equal(poll.stderr,"");assert.equal(poll.stdout,"");
 }}
 const original=readFileSync(configPath,"utf8");failure=401;bad=await run(shim,["deployment","list"]);assert(bad.err);assert.equal(bad.stdout,"");assert.equal(readFileSync(configPath,"utf8"),original);
 assert(methods.every(m=>m.startsWith("GET ")),"compatibility reads cannot post or claim");
 assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);assert(!existsSync(join(dir,jobID+"-inputs.json")));
});

// Runner code gate (wi_2be015df9af54c5c). A fixture repository whose scripts/
// and tests/ hold small stand-ins for the watched files: commit a, then
// commit b changing one of them (the runner unless another is named).
const CODE_AGENT="agt_c0defixture",CODE_RUN="run_c0defixture",SAFE_TEXT=/^[A-Za-z0-9 ,.:;()_\/-]+$/;
function codeRepo(t,real=false,changed="release-runner.mjs"){
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
function codeHost(t,cwd,extra=""){
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
const verifiedJob=(id="rel_code")=>({id,state:"verified",generation:1,commit:"c".repeat(40)});
// The gate with a spy in place of the process replacement.
function codeGate(loaded,over={}){const execs=[];return {execs,gate:runnerCodeGate({loaded,marker:null,restartedFrom:null,now:()=>Date.parse("2026-10-06T06:00:00Z"),execve:(...args)=>{execs.push(args);},...over})};}
const codeSends=h=>h.sends().filter(s=>s.requestId.startsWith("runner-code-"));

test("code a1 unchanged code does not restart: the job is claimed, nothing is re-executed and no code notice is sent",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd),{gate,execs}=codeGate(r.codeB),released=[];h.setJobs([verifiedJob()]);
 const err=await h.poll(gate,async c=>{released.push(c);});
 assert.equal(h.claims().length,1);assert.equal(execs.length,0);assert.deepEqual(codeSends(h),[]);assert.equal(err,"");
 assert.equal(h.record().state,"current");assert.deepEqual(released.map(c=>[c.job.id,c.code]),[["rel_code",{loaded:r.codeB.digest,current:r.codeB.digest}]]);
});
test("code a2 changed code cannot claim: two polls claim and set aside nothing, and the re-exec keeps argv, agent and run",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd);r.at(r.a,r.b);
 const job={...verifiedJob(),reconciliations:[{disposition:"set_aside",agentId:"agt_old",runId:"run_old"}]},journal=join(h.home,job.id+".json");
 writeFileSync(journal,JSON.stringify({jobId:job.id,commit:job.commit,agentId:"agt_old",runId:"run_old",effects:[]}));h.setJobs([job]);
 const seen=[],{gate,execs}=codeGate(r.codeA,{execve:(...args)=>{execs.push(args);seen.push({record:h.record(),head:r.head(),claims:h.claims().length});}});
 const released=[];await h.poll(gate,async c=>{released.push(c);});await h.poll(gate,async c=>{released.push(c);});
 assert.equal(h.claims().length,0);assert.deepEqual(released,[]);assert.ok(existsSync(journal));assert.deepEqual(readdirSync(h.home).filter(n=>n.includes("set-aside")),[]);
 assert.equal(execs.length,2);const [file,argv,env]=execs[0];
 assert.equal(file,process.execPath);assert.deepEqual(argv,[process.execPath,...process.execArgv,...process.argv.slice(1)]);
 assert.equal(env.TAILTERM_AGENT,CODE_AGENT);assert.equal(env.TAILTERM_RUN,CODE_RUN);assert.equal(env.TAILTERM_RUNNER_REEXEC,r.codeB.digest);assert.equal(env.TAILTERM_RUNNER_REEXEC_FROM,r.codeA.digest);
 // The record was saved, and the checkout moved to the published commit, before the re-exec.
 assert.equal(seen[0].record.state,"restart");assert.equal(seen[0].record.loaded.digest,r.codeA.digest);assert.equal(seen[0].record.current.digest,r.codeB.digest);assert.equal(seen[0].head,r.b);assert.equal(seen[0].claims,0);
 assert.equal(r.head(),r.b);assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer is restarting itself onto the published scripts"]);
});
test("code a3 active jobs drain: an owned job, a live matrix run and a blocked job each stop the restart, which then happens once before any claim",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);
 // (i) this run's own claimed job is still run.
 const own={id:"rel_own",state:"claimed",generation:2,commit:"c".repeat(40),agentId:CODE_AGENT,runId:CODE_RUN},released=[];
 h.setJobs([own,verifiedJob("rel_next")]);await h.poll(gate,async c=>{released.push(c.job.id);});
 assert.deepEqual(released,["rel_own"]);assert.equal(execs.length,0);assert.equal(r.head(),r.a);assert.equal(h.record().state,"draining");assert.equal(h.claims().length,0);
 // (ii) a matrix run this process started.
 const children=new HostAdapter({},{}).matrixChildren,key="code-a3-fixture";children.set(key,{});t.after(()=>children.delete(key));
 h.setJobs([verifiedJob("rel_next")]);await h.poll(gate,async c=>{released.push(c.job.id);});children.delete(key);
 assert.equal(execs.length,0);assert.equal(r.head(),r.a);assert.equal(h.record().state,"draining");assert.equal(h.claims().length,0);
 // (iii) a blocked job holds the fence.
 h.setJobs([{id:"rel_blocked",state:"blocked",generation:3,commit:"d".repeat(40)},verifiedJob("rel_next")]);await h.poll(gate,async c=>{released.push(c.job.id);});
 assert.equal(execs.length,0);assert.equal(r.head(),r.a);assert.equal(h.record().state,"draining");assert.equal(h.claims().length,0);assert.deepEqual(released,["rel_own"]);
 assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer code is out of date; it restarts itself after the current release"]);
 // (iv) free: one re-exec, and no claim before or after it.
 h.setJobs([verifiedJob("rel_next")]);let claimsAtExec=null;gate.execve=(...args)=>{execs.push(args);claimsAtExec=h.claims().length;};await h.poll(gate,async c=>{released.push(c.job.id);});
 assert.equal(execs.length,1);assert.equal(claimsAtExec,0);assert.equal(h.claims().length,0);assert.equal(r.head(),r.b);assert.equal(h.record().state,"restart");assert.deepEqual(released,["rel_own"]);
});
test("code a4 a runner that cannot restart refuses with one actionable notice and no claim",async t=>{
 const refusal=async(reason,arrange,expected)=>{
  const r=codeRepo(t);let cwd=r.cwd;if(reason==="published-unreadable"){cwd=mkdtempSync(join(tmpdir(),"release-code-plain-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));}
  const h=codeHost(t,cwd);r.at(r.a,r.b);h.setJobs([verifiedJob()]);const {gate,execs}=codeGate(r.codeA,arrange(r)||{});
  const err=await h.poll(gate)+await h.poll(gate);
  assert.equal(h.claims().length,0,reason);assert.equal(h.record().state,"refused",reason);assert.equal(h.record().reason,reason);
  const refused=codeSends(h).filter(s=>s.requestId.endsWith("-refused-"+reason));
  assert.equal(refused.length,1,reason);assert.match(refused[0].text,new RegExp("Reason: "+reason+"\\. Action: run tt deployment setup for the deployer with its current run as predecessor"));
  assert.deepEqual(codeSends(h).map(s=>s.requestId.split("-").slice(5).join("-")),expected.notices,reason);
  assert.deepEqual(err.split("\n").filter(Boolean),expected.lines,reason);
  return {r,h,execs};
 };
 const refusedLine="Deployer code is out of date and it cannot restart itself; no release is claimed until it is re-provisioned.",restartLine="Deployer restarting itself onto the published scripts.";
 // The restart is announced before the exec that then fails, so this case alone has two notices and two lines; the exec is tried once.
 const failed=await refusal("exec-failed",()=>{let n=0;return {execve:()=>{n++;throw new Error("SYNTHETIC exec failure "+n);}};},{notices:["restart","refused-exec-failed"],lines:[restartLine,refusedLine]});
 assert.equal(failed.r.head(),failed.r.b);
 const dirty=await refusal("checkout-dirty",r=>{writeFileSync(join(r.cwd,"untracked.txt"),"x");},{notices:["refused-checkout-dirty"],lines:[refusedLine]});
 assert.equal(dirty.r.head(),dirty.r.a);assert.equal(dirty.execs.length,0);
 const looped=await refusal("restart-did-not-refresh",r=>({marker:r.codeB.digest}),{notices:["refused-restart-did-not-refresh"],lines:[refusedLine]});
 assert.equal(looped.r.head(),looped.r.a);assert.equal(looped.execs.length,0);
 const plain=await refusal("published-unreadable",()=>{},{notices:["refused-published-unreadable"],lines:[refusedLine]});assert.equal(plain.execs.length,0);
 // Ignored build outputs are not a dirty checkout.
 const r=codeRepo(t);r.at(r.a,r.b);writeFileSync(join(r.cwd,".git/info/exclude"),"node_modules/\n");mkdirSync(join(r.cwd,"node_modules"));writeFileSync(join(r.cwd,"node_modules/x"),"x");prepareCode(r.cwd,r.codeB);assert.equal(r.head(),r.b);
 // With an unreadable published ref and nothing to claim, the poll stays silent.
 const empty=mkdtempSync(join(tmpdir(),"release-code-plain-"));t.after(()=>rmSync(empty,{recursive:true,force:true}));
 const quiet=codeHost(t,empty);assert.equal(await quiet.poll(codeGate(r.codeA).gate),"");assert.deepEqual(quiet.sends(),[]);assert.equal(quiet.record().reason,"published-unreadable");
});
test("code a5 the published ref, not the working tree, is what counts as current",async t=>{
 // The checkout sits on an unpublished commit that changes a watched script; tasks-hub is what was loaded.
 const r=codeRepo(t),h=codeHost(t,r.cwd),{gate,execs}=codeGate(r.codeA);r.at(r.b,r.a);h.setJobs([verifiedJob()]);
 assert.notEqual(diskCode(join(r.cwd,"scripts")).digest,r.codeA.digest);
 assert.equal(await h.poll(gate),"");assert.equal(h.claims().length,1);assert.equal(execs.length,0);assert.equal(r.head(),r.b);assert.equal(h.record().state,"current");
 // The disk still holds what was loaded, but tasks-hub moved a watched script.
 const s=codeRepo(t),g=codeHost(t,s.cwd);s.at(s.a,s.b);g.setJobs([verifiedJob()]);assert.equal(diskCode(join(s.cwd,"scripts")).digest,s.codeA.digest);
 const stale=codeGate(s.codeA,{prepare:()=>{throw releaseError("checkout-dirty");}});await g.poll(stale.gate);
 assert.equal(g.claims().length,0);assert.equal(stale.execs.length,0);assert.equal(g.record().state,"refused");assert.deepEqual(g.record().changed,["release-runner.mjs"]);
});
test("code a6 the private record names loaded and current code, is 0600 and is not rewritten by an identical poll; the job journal names both digests",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd);r.at(r.a,r.b);let now=Date.parse("2026-10-06T06:00:00Z");
 const own={id:"rel_own",state:"claimed",generation:2,commit:"c".repeat(40),agentId:CODE_AGENT,runId:CODE_RUN};h.setJobs([own]);
 const {gate}=codeGate(r.codeA,{now:()=>now});await h.poll(gate);
 const path=join(h.home,"runner-code.json"),first=statSync(path),record=h.record();
 assert.equal(first.mode&0o777,0o600);
 assert.deepEqual(record,{version:1,agentId:CODE_AGENT,runId:CODE_RUN,pid:process.pid,startedAt:"2026-10-06T06:00:00.000Z",checkedAt:"2026-10-06T06:00:00.000Z",state:"draining",
  loaded:{digest:r.codeA.digest,files:r.codeA.files},current:{commit:r.b,digest:r.codeB.digest,files:r.codeB.files},changed:["release-runner.mjs"]});
 assert.equal(record.loaded.digest,codeDigest(record.loaded.files));assert.notEqual(record.loaded.digest,record.current.digest);
 now+=30000;await h.poll(gate);assert.equal(statSync(path).ino,first.ino);assert.deepEqual(h.record(),record);
 // A changed state is a new record; reason and restartedFrom appear only when they apply.
 h.setJobs([]);await h.poll(codeGate(r.codeA,{now:()=>now,marker:r.codeB.digest}).gate);
 assert.deepEqual(Object.keys(h.record()),["version","agentId","runId","pid","startedAt","checkedAt","state","reason","loaded","current","changed"]);assert.equal(h.record().checkedAt,"2026-10-06T06:00:30.000Z");
 const after=codeRecord(h.config,{loaded:r.codeB,marker:r.codeB.digest,restartedFrom:r.codeA.digest,now:()=>now},{state:"current"},r.codeB);
 assert.equal(after.restartedFrom,r.codeA.digest);assert.deepEqual(after.changed,[]);assert.equal(h.record().restartedFrom,r.codeA.digest);
 // runRelease keeps the digests in the job journal.
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),c={...config(f,j),code:{loaded:r.codeA.digest,current:r.codeB.digest}};
 assert.equal((await runRelease(c,fake())).outcome,"released");assert.deepEqual(JSON.parse(readFileSync(c.journalPath,"utf8")).code,{loaded:r.codeA.digest,current:r.codeB.digest});
});
test("code a7 every code notice is safe text with 12-hex digests and only watched file names, under a request id a resend repeats",t=>{
 const r=codeRepo(t),home=mkdtempSync(join(tmpdir(),"release-code-notice-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const saved=process.env.TAILTERM_RUN;process.env.TAILTERM_RUN=CODE_RUN;t.after(()=>{if(saved===undefined)delete process.env.TAILTERM_RUN;else process.env.TAILTERM_RUN=saved;});
 const build=(decision,over={})=>codeRecord({journalDirectory:home},{loaded:r.codeA,marker:null,restartedFrom:null,now:()=>0,...over},decision,r.codeB);
 const l=r.codeA.digest.slice(0,12),c=r.codeB.digest.slice(0,12),ids=new Set();
 const cases=[[build({state:"draining"}),"draining"],[build({state:"restart"}),"restart"],...CODE_REASONS.map(reason=>[build({state:"refused",reason}),"refused"]),
  [codeRecord({journalDirectory:home},{loaded:r.codeB,marker:r.codeB.digest,restartedFrom:r.codeA.digest,now:()=>0},{state:"current"},r.codeB),"restarted"]];
 for(const [record,kind] of cases){
  const notice=codeNotice(record,kind);
  assert.match(notice.text,SAFE_TEXT);assert.match(notice.subject,SAFE_TEXT);assert.ok(notice.subject.length<=120);assert.ok(Buffer.byteLength(notice.text)<1000);assert.match(notice.requestId,/^[A-Za-z0-9_-]{1,128}$/);
  assert.ok(notice.text.includes(kind==="restarted"?c:l));assert.ok(notice.text.includes(c));assert.ok(notice.text.includes(r.b.slice(0,12)));
  assert.ok(!/[a-f0-9]{13}/.test(notice.text),"no digest longer than its 12-hex prefix");
  for(const name of notice.text.match(/[A-Za-z0-9_.\/-]+\.mjs/g)||[])assert.ok(RUNNER_CODE_FILES.includes(name),name);
  if(kind!=="restarted")assert.ok(notice.text.includes("Changed: release-runner.mjs."));
  if(kind==="refused")assert.ok(notice.text.includes(`Reason: ${record.reason}.`));
  // The same state from a restarted process is the same request.
  assert.deepEqual(codeNotice(JSON.parse(JSON.stringify({...record,pid:1,startedAt:"x",checkedAt:"y"})),kind),notice);assert.ok(!ids.has(notice.requestId));ids.add(notice.requestId);
 }
 // Unlisted names, reasons and run ids never reach the text.
 const hostile=codeNotice({state:"refused",reason:"x'; rm -rf /",runId:"run with spaces'",loaded:{digest:"SECRET"},current:{commit:"../../etc/passwd",digest:r.codeB.digest},changed:["../../etc/passwd","release-runner.mjs","evil.mjs"]});
 assert.match(hostile.text,SAFE_TEXT);assert.ok(!/passwd|evil|rm -rf|SECRET|spaces/.test(hostile.text+hostile.requestId));assert.ok(hostile.text.includes("Reason: unclassified."));assert.ok(hostile.text.includes("Changed: release-runner.mjs."));
 assert.equal(codeNotice({state:"current"}),null);
 // The decision itself, with a fixed clock and digests only.
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeA,idle:false}),{state:"current"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:false}),{state:"draining"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:true,marker:null}),{state:"restart"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:true,marker:r.codeB.digest}),{state:"refused",reason:"restart-did-not-refresh"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:null,idle:true}),{state:"refused",reason:"published-unreadable"});
 assert.deepEqual(codeDecision({loaded:null,published:r.codeB,idle:true}),{state:"refused",reason:"loaded-unreadable"});
});
test("code a8 a real re-exec keeps the process, agent and run, and claims once only after the checkout moved",async t=>{
 const r=codeRepo(t,true),h=codeHost(t,r.cwd);r.at(r.a,r.b);h.setJobs([verifiedJob()]);
 const configPath=join(h.home,"config.json");writeFileSync(configPath,JSON.stringify(h.config));
 const env={...process.env,TAILTERM_AGENT:CODE_AGENT,TAILTERM_RUN:CODE_RUN};delete env.TAILTERM_RUNNER_REEXEC;delete env.TAILTERM_RUNNER_REEXEC_FROM;
 const run=spawnSync(process.execPath,[join(r.cwd,"scripts/release-runner.mjs"),"--config",configPath,"--once"],{cwd:r.cwd,env,encoding:"utf8",stdio:["ignore","pipe","pipe"],timeout:60000});
 assert.equal(run.status,0,run.stderr);assert.equal(run.stdout,"");assert.match(run.stderr,/^Deployer restarting itself onto the published scripts\.\n/);
 const calls=h.calls(),claims=h.claims();
 assert.equal(claims.length,1);assert.equal(claims[0].head,r.b);assert.equal(calls[0].head,r.a);
 // One process throughout: every call has the pid the first list call saw, and the same agent and run.
 assert.equal(claims[0].ppid,calls[0].ppid);assert.ok(calls.every(c=>c.ppid===calls[0].ppid && c.run===CODE_RUN && c.agent===CODE_AGENT));
 const before=calls.slice(0,calls.indexOf(claims[0]));assert.ok(before.some(c=>c.head===r.a) && before.some(c=>c.head===r.b));
 const record=h.record();
 assert.equal(record.state,"current");assert.equal(record.loaded.digest,record.current.digest);assert.equal(record.current.commit,r.b);assert.equal(record.restartedFrom,r.codeA.digest);assert.equal(record.pid,calls[0].ppid);assert.equal(record.runId,CODE_RUN);
 assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer is restarting itself onto the published scripts","Deployer now runs the published scripts"]);assert.equal(r.head(),r.b);
});
test("code a9 a failed CLI call keeps its exact argv, exit code and stderr privately, and only the subcommand and exit code elsewhere",async t=>{
 const marker="SYNTHETIC_PRIVATE_STDERR",cwd=mkdtempSync(join(tmpdir(),"release-cli-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const h=codeHost(t,cwd,`if(a[1]==='refuse'){fs.writeSync(2,${JSON.stringify(marker+" refuse failed\n")});process.exit(3);}`);h.setJobs([{...verifiedJob("rel_cli"),verificationDigest:"a".repeat(64)}]);
 const journal=join(h.home,"rel_cli.json"),failures=()=>JSON.parse(readFileSync(join(h.home,"cli-failures.json"),"utf8"));
 // No base commit: integration refuses before publication, then the refuse call itself fails.
 const err=await h.poll(null,runRelease);assert.equal(err,"Release held; inspect handler fence and private journal.\n");
 const state=JSON.parse(readFileSync(journal,"utf8"));
 assert.equal(state.phase,"refusing");assert.equal(state.refusalReason,"Release binding mismatch");assert.equal(state.refuseFailure,"tt deployment refuse exit 3");assert.deepEqual(state.effects,[]);
 assert.equal(statSync(join(h.home,"cli-failures.json")).mode&0o777,0o600);
 let kept=failures();assert.equal(kept.version,1);assert.equal(kept.failures.length,1);const entry=kept.failures[0];
 assert.deepEqual(entry.argv,[h.tt,"deployment","refuse","--job","rel_cli","--generation","2","--request-id","rel_cli-refuse-2"]);
 assert.equal(entry.exitCode,3);assert.equal(entry.signal,null);assert.equal(entry.stderr,marker+" refuse failed\n");assert.equal(entry.stderrBytes,Buffer.byteLength(entry.stderr));assert.equal(entry.stderrTruncated,false);
 assert.equal(entry.jobId,"rel_cli");assert.equal(entry.agentId,CODE_AGENT);assert.equal(entry.runId,CODE_RUN);assert.equal(entry.cwd,cwd);assert.equal(entry.count,1);
 // Three identical failures are one entry counted three times, under one request id.
 rmSync(journal);await h.poll(null,runRelease);rmSync(journal);await h.poll(null,runRelease);
 kept=failures();assert.equal(kept.failures.length,1);assert.equal(kept.failures[0].count,3);assert.equal(kept.failures[0].at,entry.at);
 const sent=h.sends();assert.equal(sent.length,3);assert.equal(new Set(sent.map(s=>s.requestId)).size,1);
 for(const c of h.calls())for(const text of c.argv)assert.ok(!text.includes(marker));
 for(const s of sent){assert.ok(s.text.includes("tt deployment refuse exit 3"));assert.match(s.text,SAFE_TEXT);assert.equal(s.subject,"A deployer CLI call failed");}
 assert.deepEqual(cliFailureNotice({reason:"tt deployment refuse exit 3",jobId:"rel_cli",at:entry.at},CODE_RUN).requestId,sent[0].requestId);
 // Where the CLI call is itself the cause, the poll's reason names its subcommand.
 const adapter=new HostAdapter({cwd,tt:h.tt,journalDirectory:h.home},{id:"rel_cli"});
 assert.throws(()=>adapter.command([h.tt,"deployment","refuse","--job","rel_cli"]),reasonIs("tt deployment refuse exit 3"));
 assert.throws(()=>adapter.command([h.tt,"deployment","--job","rel_cli"]),reasonIs("tt deployment exit 2"));
 assert.throws(()=>adapter.command([h.tt,"Deployment","refuse"]),reasonIs("tt exit 3"));
 // A long stderr keeps its last 16384 bytes.
 const big=codeHost(t,cwd,`fs.writeSync(2,'HEAD_OF_STDERR'+'x'.repeat(300*1024-14-5)+'TAIL.');process.exit(4);`),loud=new HostAdapter({cwd,tt:big.tt,journalDirectory:big.home},{});
 assert.throws(()=>loud.command([big.tt,"deployment","handler"]),reasonIs("tt deployment handler exit 4"));
 const long=JSON.parse(readFileSync(join(big.home,"cli-failures.json"),"utf8")).failures[0];
 assert.equal(Buffer.byteLength(long.stderr),16384);assert.equal(long.stderrBytes,300*1024);assert.equal(long.stderrTruncated,true);assert.ok(long.stderr.endsWith("TAIL."));assert.ok(!long.stderr.includes("HEAD_OF_STDERR"));
 assert.deepEqual(loud.cliFailures.map(f=>f.reason),["tt deployment handler exit 4"]);
 // Another program, or no journal directory, records nothing; at most twenty distinct failures are kept.
 const before=readFileSync(join(big.home,"cli-failures.json"),"utf8");
 assert.throws(()=>loud.command([process.execPath,"-e","process.stderr.write('other');process.exit(5)"]),reasonIs("node exit 5"));assert.equal(readFileSync(join(big.home,"cli-failures.json"),"utf8"),before);
 const bare=mkdtempSync(join(tmpdir(),"release-cli-bare-"));t.after(()=>rmSync(bare,{recursive:true,force:true}));
 assert.throws(()=>new HostAdapter({cwd:bare,tt:big.tt},{}).command([big.tt,"deployment","handler"]),reasonIs("tt deployment handler exit 4"));assert.deepEqual(readdirSync(bare),[]);
 for(let i=0;i<25;i++)assert.throws(()=>loud.command([big.tt,"deployment","get","--job","rel_"+i]),reasonIs("tt deployment get exit 4"));
 const capped=JSON.parse(readFileSync(join(big.home,"cli-failures.json"),"utf8")).failures;assert.equal(capped.length,20);assert.equal(capped.at(-1).argv.at(-1),"rel_24");
});
// Every "./" or "../" import of the watched files that is not itself watched,
// as "importer imports path" with paths from the repository root.
function unwatchedImports(watched,directory=dirname(RUNNER)){
 const root=dirname(directory),paths=new Set(watched.map(n=>resolve(directory,n))),found=[];
 for(const file of paths){
  const specifiers=[...readFileSync(file,"utf8").matchAll(/(?:\bfrom\s*|\bimport\s*\(?\s*)["'](\.\.?\/[^"']+)["']/g)].map(m=>m[1]);
  for(const specifier of specifiers){const imported=resolve(dirname(file),specifier);if(!paths.has(imported))found.push(`${file.slice(root.length+1)} imports ${imported.slice(root.length+1)}`);}
 }
 return found;
}
test("code a10 every relative import of the watched files is itself watched, and this process recorded its own code",()=>{
 const directory=dirname(RUNNER);
 assert.deepEqual(unwatchedImports(RUNNER_CODE_FILES),[]);
 assert.equal(RUNNER_CODE_FILES.length,7);assert.deepEqual([...RUNNER_CODE_FILES].sort(),RUNNER_CODE_FILES);
 // The guard sees an import that leaves scripts/: without its entry the matrix's import is reported.
 assert.deepEqual(unwatchedImports(RUNNER_CODE_FILES.filter(n=>n!=="../tests/test-binaries.mjs")),["scripts/verify-matrix.mjs imports tests/test-binaries.mjs"]);
 assert.deepEqual(unwatchedImports(RUNNER_CODE_FILES.filter(n=>n!=="release-probe.mjs")),["scripts/release-inputs.mjs imports scripts/release-probe.mjs","scripts/release-runner.mjs imports scripts/release-probe.mjs"]);
 assert.deepEqual(LOADED_CODE,diskCode(directory));assert.equal(LOADED_CODE.digest,codeDigest(LOADED_CODE.files));
 assert.match(LOADED_CODE.files["../tests/test-binaries.mjs"],/^[a-f0-9]{64}$/);
 assert.ok(readFileSync(RUNNER,"utf8").includes("code:runnerCodeGate()"),"only the daemon entry builds the real gate");
});
test("code a11 a change to the test binaries module alone is stale code: it drains while a release is active and restarts once idle",async t=>{
 const name="../tests/test-binaries.mjs",r=codeRepo(t,false,name),h=codeHost(t,r.cwd),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);
 // Only that file differs between the two commits, and both readers of the code see it.
 assert.deepEqual(git(r.cwd,"diff","--name-only",r.a,r.b).split("\n"),["tests/test-binaries.mjs"]);
 assert.deepEqual(RUNNER_CODE_FILES.filter(n=>r.codeA.files[n]!==r.codeB.files[n]),[name]);assert.notEqual(r.codeA.digest,r.codeB.digest);
 assert.deepEqual(diskCode(join(r.cwd,"scripts")),{files:r.codeA.files,digest:r.codeA.digest});
 const others=Object.fromEntries(RUNNER_CODE_FILES.map(n=>[n,"0".repeat(64)]));assert.notEqual(codeDigest(others),codeDigest({...others,[name]:"1".repeat(64)}));
 // An active release: its own job still runs, nothing new is claimed and nothing is re-executed.
 const own={id:"rel_own",state:"claimed",generation:2,commit:"c".repeat(40),agentId:CODE_AGENT,runId:CODE_RUN},released=[];
 h.setJobs([own,verifiedJob("rel_next")]);await h.poll(gate,async c=>{released.push(c.job.id);});
 assert.deepEqual(released,["rel_own"]);assert.equal(execs.length,0);assert.equal(h.claims().length,0);assert.equal(r.head(),r.a);
 assert.equal(h.record().state,"draining");assert.deepEqual(h.record().changed,[name]);
 assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer code is out of date; it restarts itself after the current release"]);
 assert.ok(codeSends(h)[0].text.includes(`Changed: ${name}.`));assert.match(codeSends(h)[0].text,SAFE_TEXT);
 // Idle: one re-exec aimed at the published code, from the published checkout, and still no claim.
 h.setJobs([verifiedJob("rel_next")]);await h.poll(gate,async c=>{released.push(c.job.id);});
 assert.equal(execs.length,1);assert.equal(execs[0][2].TAILTERM_RUNNER_REEXEC,r.codeB.digest);assert.equal(execs[0][2].TAILTERM_RUNNER_REEXEC_FROM,r.codeA.digest);
 assert.equal(h.claims().length,0);assert.deepEqual(released,["rel_own"]);assert.equal(r.head(),r.b);assert.equal(h.record().state,"restart");assert.deepEqual(h.record().changed,[name]);
 assert.equal(readFileSync(join(r.cwd,"tests/test-binaries.mjs"),"utf8"),`// ${name} a\n// b\n`);
 assert.ok(codeSends(h).at(-1).text.includes(`Changed: ${name}.`));
});
test("code a12 a restart begun by a runner that watched only the scripts is still announced, and its marker decides nothing else",async t=>{
 const name="../tests/test-binaries.mjs",r=codeRepo(t,false,name),scripts=RUNNER_CODE_FILES.filter(n=>n!==name);assert.equal(scripts.length,6);
 // What the earlier runner computed: the sha256 of the six scripts' "name:sha256" lines.
 const earlier=code=>createHash("sha256").update(scripts.map(n=>`${n}:${code.files[n]}`).join("\n")).digest("hex"),from="f".repeat(64);
 assert.notEqual(earlier(r.codeB),r.codeB.digest);
 // Restarted onto b with that marker: current, one job claimed, and the restarted notice is sent once.
 const h=codeHost(t,r.cwd),{gate,execs}=codeGate(r.codeB,{marker:earlier(r.codeB),restartedFrom:from});h.setJobs([verifiedJob()]);
 await h.poll(gate);await h.poll(gate);
 assert.equal(h.record().state,"current");assert.equal(h.record().restartedFrom,from);assert.equal(execs.length,0);assert.equal(h.claims().length,2);
 assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer now runs the published scripts"]);assert.ok(codeSends(h)[0].text.includes(`from scripts ${from.slice(0,12)} and now runs ${r.codeB.digest.slice(0,12)}`));
 // Any other marker announces nothing.
 const home=mkdtempSync(join(tmpdir(),"release-code-marker-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const record=marker=>codeRecord({journalDirectory:home},{loaded:r.codeB,marker,restartedFrom:from,now:()=>0},{state:"current"},r.codeB);
 assert.equal(record("e".repeat(64)).restartedFrom,undefined);assert.equal(record(null).restartedFrom,undefined);assert.equal(record(r.codeB.digest).restartedFrom,from);
 // The decision compares the marker with the published digest only: the six-script value never refuses or allows a restart.
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:true,marker:earlier(r.codeB)}),{state:"restart"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:true,marker:r.codeB.digest}),{state:"refused",reason:"restart-did-not-refresh"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:false,marker:earlier(r.codeB)}),{state:"draining"});
});
test("code b1 a stop signal during the restart poll means no re-exec and no claim, and the runner ends",{timeout:20000},async t=>{
 // The signal arrives during the poll's synchronous work and is handled at the next turn of the event loop.
 // (i) before the checkout is touched: nothing moves.
 const r=codeRepo(t),h=codeHost(t,r.cwd),early=new AbortController();r.at(r.a,r.b);h.setJobs([verifiedJob()]);
 const first=codeGate(r.codeA,{published:cwd=>{setImmediate(()=>early.abort());return publishedCode(cwd);}});
 assert.equal(await h.poll(first.gate,undefined,{signal:early.signal}),"");
 assert.equal(first.execs.length,0);assert.equal(h.claims().length,0);assert.equal(r.head(),r.a);assert.deepEqual(h.sends(),[]);
 // (ii) during the wait inside exec, after the restart was announced: the process is not replaced, and the loop is left without another poll.
 const s=codeRepo(t),g=codeHost(t,s.cwd),late=new AbortController();s.at(s.a,s.b);g.setJobs([verifiedJob()]);
 const second=codeGate(s.codeA),exec=second.gate.exec;second.gate.exec=function(...args){setImmediate(()=>late.abort());return exec.apply(this,args);};
 await g.poll(second.gate,undefined,{signal:late.signal});
 assert.equal(second.execs.length,0);assert.equal(g.claims().length,0);assert.equal(g.calls().filter(c=>c.argv[1]==="list"&&!c.argv.includes("--snapshot")).length,1);
 // Without a signal the same gate does re-exec.
 const u=codeRepo(t),k=codeHost(t,u.cwd),idle=new AbortController();u.at(u.a,u.b);const third=codeGate(u.codeA);
 await k.poll(third.gate,undefined,{once:true,signal:idle.signal});assert.equal(third.execs.length,1);
});

// Release batching (wi_50e80d45647b342a slice one).
import {integrateBatch,batchCandidates,batchAcceptedChecks,batchMismatch,batchPlanCovers,batchId,releaseBatchPolicy,materializeBatch,readBatchSolo,addBatchSolo,reconcileReceipts,BATCH_IMPORT_WAIT_MS} from "../scripts/release-runner.mjs";
import {planWithPreservation,digest as matrixDigest} from "../scripts/verify-matrix.mjs";
// One job per file, each a single commit on the published base; the first is
// the claimed lead. tasks-hub then moves, so every job is cherry-picked.
function batchJobs(files,{moved="client/hub.js"}={}){
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
function batchFake(lead,{refuse=()=>false}={}){
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
function batchConfig(b,max,over={}){
 const home=mkdtempSync(join(tmpdir(),"release-batch-")),solo=[];
 return {cwd:b.f.cwd,job:b.lead,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,b.f.base])),journalPath:join(home,b.lead.id+".json"),testPolicy:{startupMs:0,failures:1,intervalMs:0,relayCleanMs:0},home,solo,
  batch:{max,importWaitMs:BATCH_IMPORT_WAIT_MS,candidates:b.members,onFallback:ids=>{solo.push(...ids);addBatchSolo(home,ids);},...over}};
}
const batchJournal=c=>JSON.parse(readFileSync(c.journalPath,"utf8"));
const batchFiles=home=>readdirSync(home).filter(n=>n.startsWith("bat_")||n==="batch-solo.json");
const three=()=>batchJobs([["client/a.js","a"],["client/b.js","b"],["client/c.js","c"]]);
const clean=cwd=>assert.equal(git(cwd,"status","--porcelain"),"");
test("batch1 a clean batch of three integrates once, verifies once and deploys once",async()=>{
 const b=three(),a=batchFake(b.lead),c=batchConfig(b,3);
 const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");
 assert.deepEqual(a.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3","verify","merged","deploy:tailos","finish"]);
 const journal=batchJournal(c),batch=journal.batch,integrated=git(b.f.cwd,"rev-parse","refs/heads/tasks-hub");
 assert.equal(batch.id,batchId("rel_b1",2,b.tip));assert.match(batch.id,/^bat_[a-f0-9]{16}$/);assert.equal(batch.base,b.tip);assert.equal(journal.expected,b.tip);
 assert.deepEqual(batch.jobs.map(j=>j.jobId),["rel_b1","rel_b2","rel_b3"]);
 let from=b.tip;
 for(const [i,range] of batch.jobs.entries()){
  assert.equal(range.from,from);assert.deepEqual(git(b.f.cwd,"diff","--name-only",range.from,range.to).split("\n"),[`client/${"abc"[i]}.js`]);
  assert.equal(range.entryId,`tqe_b${i+1}`);assert.equal(range.verificationDigest,String(i+1).repeat(64));from=range.to;
 }
 assert.equal(from,integrated);assert.equal(r.commit,integrated);assert.equal(journal.integrated,integrated);
 assert.deepEqual(a.ops.map(o=>[o.op,o.entryId,o.commit,o.generation]),[["batch-open",undefined,b.tip,2],["batch-add","tqe_b1",batch.jobs[0].to,3],["batch-add","tqe_b2",batch.jobs[1].to,4],["batch-add","tqe_b3",integrated,5]]);
 assert.equal(a.verifies.length,1);assert.equal(a.verifies[0].integratedCommit,integrated);
 assert.deepEqual(journal.batchOps.map(o=>[o.op,o.status,o.requestId]),[["batch-open","done","rel_b1-batch-open-2"],["batch-add","done","rel_b1-batch-add-3"],["batch-add","done","rel_b1-batch-add-4"],["batch-add","done","rel_b1-batch-add-5"]]);
 // a11: the batch has its own receipt, every member a journal naming the
 // batch, and the hub receipt the lead sent is the lead's own.
 const receipt=JSON.parse(readFileSync(join(c.home,batch.id+"-receipt.json"),"utf8"));
 assert.equal(receipt.batchId,batch.id);assert.equal(receipt.leadJobId,"rel_b1");assert.equal(receipt.outcome,"released");assert.equal(receipt.base,b.tip);assert.equal(receipt.integrated,integrated);assert.equal(receipt.commit,integrated);
 assert.deepEqual(receipt.jobs,batch.jobs);assert.deepEqual(receipt.targets.map(t=>[t.target,t.outcome]),[["tailos","released"]]);assert.deepEqual(receipt.dropped,[]);
 for(const [i,id] of ["rel_b2","rel_b3"].entries()){
  const member=JSON.parse(readFileSync(join(c.home,id+".json"),"utf8"));
  assert.equal(member.phase,"complete");assert.deepEqual(member.batch,{id:batch.id,leadJobId:"rel_b1",from:batch.jobs[i+1].from,to:batch.jobs[i+1].to,integrated});
  assert.equal(member.receipt.jobId,id);assert.equal(member.receipt.verificationDigest,String(i+2).repeat(64));assert.equal(member.receipt.commit,integrated);assert.equal(member.commit,b.members[i].commit);
 }
 assert.equal(a.receipts.length,1);assert.equal(a.receipts[0].jobId,"rel_b1");assert.equal(a.receipts[0].verificationDigest,"1".repeat(64));assert.equal(a.receipts[0].batchId,undefined);
 assert.equal(journal.batchReceipt.batchId,batch.id);assert.deepEqual(c.solo,[]);clean(b.f.cwd);
 // A repeated run returns the saved receipt and repeats nothing.
 const calls=a.calls.length;assert.deepEqual(await runRelease(c,a),r);assert.equal(a.calls.length,calls);
});
test("batch2 a conflicting job drops out and waits",async()=>{
 const b=batchJobs([["client/a.js","a"],["client/base.js","candidate"],["client/c.js","c"]],{moved:"client/base.js"}),a=batchFake(b.lead),c=batchConfig(b,3);
 const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");
 assert.deepEqual(a.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b3","verify","merged","deploy:tailos","finish"]);assert.deepEqual(a.refused,[]);
 const journal=batchJournal(c);
 assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b3"]);
 assert.deepEqual(journal.batchDropped,[{jobId:"rel_b2",reason:"integration-conflict: client/base.js"}]);
 assert.equal(journal.batch.jobs[1].from,journal.batch.jobs[0].to);
 clean(b.f.cwd);assert.equal(git(b.f.cwd,"rev-parse","HEAD"),r.commit);
 assert.ok(!existsSync(join(c.home,"rel_b2.json")),"no journal for the dropped job");assert.ok(existsSync(join(c.home,"rel_b3.json")));
 assert.equal(git(b.f.cwd,"show",r.commit+":client/base.js"),"release");
 assert.deepEqual(JSON.parse(readFileSync(join(c.home,journal.batch.id+"-receipt.json"),"utf8")).dropped,journal.batchDropped);
 assert.deepEqual(c.solo,[],"a dropped job is not marked to release alone");
});
test("batch3 a failing matrix falls back to per-job releases",async()=>{
 const b=three(),a=batchFake(b.lead),c=batchConfig(b,3);
 a.verifyIntegrated=async j=>{a.calls.push("verify");a.verifies.push(j);if(a.verifies.length===1)throw releaseError("Integrated matrix receipt is not eligible");return true;};
 const refs=[];const publish=a.merged;a.merged=async commit=>{refs.push(git(b.f.cwd,"rev-parse","refs/heads/tasks-hub"));return publish(commit);};
 const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");
 assert.deepEqual(a.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3","verify","batch-drop","verify","merged","deploy:tailos","finish"]);
 assert.ok(!a.calls.includes("refuse"));
 const journal=batchJournal(c),batchTip=a.verifies[0].integratedCommit,alone=a.verifies[1].integratedCommit;
 assert.notEqual(batchTip,alone);assert.equal(r.commit,alone);assert.equal(git(b.f.cwd,"rev-parse","refs/heads/tasks-hub"),alone);
 assert.deepEqual(refs,[alone]);assert.throws(()=>git(b.f.cwd,"merge-base","--is-ancestor",batchTip,"refs/heads/tasks-hub"),"tasks-hub never carried the batch commit");
 assert.deepEqual(git(b.f.cwd,"diff","--name-only",b.tip,alone).split("\n"),["client/a.js"]);
 assert.equal(journal.batch,null);assert.equal(journal.batchReceipt,undefined);
 assert.deepEqual(journal.batchFallback,{batchId:batchId("rel_b1",2,b.tip),reason:"batch-matrix-failed",detail:"Integrated matrix receipt is not eligible",members:["rel_b2","rel_b3"],status:"done"});
 assert.deepEqual(c.solo,["rel_b2","rel_b3"]);assert.deepEqual([...readBatchSolo(c.home)],["rel_b2","rel_b3"]);
 assert.deepEqual(batchFiles(c.home),["batch-solo.json"],"no batch receipt for a batch that never published");assert.ok(!existsSync(join(c.home,"rel_b2.json")));
 // Each member is then released alone: it leads no batch and joins none.
 const summaries=[b.lead,...b.members].map((j,i)=>({...j,summary:true,rowId:i+1}));
 for(const [i,member] of b.members.entries()){
  const lead={...member,state:"claimed",generation:2,agentId:"agt_fixture",runId:"run_fixture"};
  assert.deepEqual(batchCandidates(summaries,lead,readBatchSolo(c.home)),[]);
  const a2=batchFake(lead),c2=batchConfig({f:b.f,lead,members:[]},3);
  const r2=await runRelease(c2,a2);
  assert.equal(r2.outcome,"released");assert.deepEqual(a2.calls,["verify","merged","deploy:tailos","finish"]);assert.equal(batchJournal(c2).batch,undefined);
  assert.equal(git(b.f.cwd,"show",r2.commit+`:client/${"bc"[i]}.js`),"bc"[i]);
 }
});
test("batch3b a fallback waits until the batch's own matrix run is confirmed over",async()=>{
 const b=three(),a=batchFake(b.lead),c=batchConfig(b,3);let settled=true;const asked=[];
 a.settleMatrixRuns=async()=>{asked.push(settled);return settled;};
 a.verifyIntegrated=async j=>{a.calls.push("verify");a.verifies.push(j);if(a.verifies.length===1)throw releaseError("Host operation failed");return true;};
 const first=await runRelease(c,a);assert.equal(first.outcome,"released");assert.deepEqual(asked,[true,true]);
 // Not over: nothing is dropped and the checkout stays on the batch.
 const b2=three(),a2=batchFake(b2.lead),c2=batchConfig(b2,3);let over=true;
 a2.settleMatrixRuns=async()=>over;
 a2.verifyIntegrated=async j=>{a2.calls.push("verify");a2.verifies.push(j);if(a2.verifies.length===1){over=false;throw releaseError("Host operation failed");}return true;};
 assert.deepEqual(await runRelease(c2,a2),{jobId:"rel_b1",outcome:"waiting_matrix"});
 let journal=batchJournal(c2);const batchTip=journal.batch.jobs.at(-1).to;
 assert.equal(journal.batchFallback.status,"dropping");assert.ok(!a2.calls.includes("batch-drop"));assert.equal(git(b2.f.cwd,"rev-parse","HEAD"),batchTip);assert.deepEqual(c2.solo,[]);
 assert.deepEqual(await runRelease(c2,a2),{jobId:"rel_b1",outcome:"waiting_matrix"});assert.ok(!a2.calls.includes("batch-drop"));
 over=true;const r=await runRelease(c2,a2);
 assert.equal(r.outcome,"released");assert.deepEqual(a2.calls.slice(-5),["batch-drop","verify","merged","deploy:tailos","finish"]);assert.deepEqual(c2.solo,["rel_b2","rel_b3"]);
 journal=batchJournal(c2);assert.equal(journal.batchFallback.status,"done");assert.equal(journal.batchFallback.reason,"batch-matrix-failed");assert.equal(journal.batchFallback.detail,"Host operation failed");
 assert.deepEqual(git(b2.f.cwd,"diff","--name-only",b2.tip,r.commit).split("\n"),["client/a.js"]);
});
test("batch4 a rollback restores the whole batch",async()=>{
 const b=three(),a=batchFake(b.lead),c=batchConfig(b,3);a.check=async()=>"identity";
 await assert.rejects(runRelease(c,a),/failed/);
 assert.deepEqual(a.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3","verify","merged","deploy:tailos","rollback:tailos","escalate","bug","finish"]);
 const journal=batchJournal(c),revert=git(b.f.cwd,"rev-parse","refs/heads/tasks-hub"),integrated=journal.batch.jobs.at(-1).to;
 assert.equal(git(b.f.cwd,"rev-parse",revert+"^"),integrated);assert.equal(git(b.f.cwd,"rev-parse",revert+"^{tree}"),git(b.f.cwd,"rev-parse",b.tip+"^{tree}"));
 for(const file of ["a","b","c"])assert.equal(git(b.f.cwd,"ls-tree","--name-only",revert,`client/${file}.js`),"");
 assert.equal(a.receipts.length,1);assert.equal(a.receipts[0].outcome,"rolled_back");assert.equal(a.receipts[0].jobId,"rel_b1");assert.deepEqual(a.receipts[0].revert,{commit:revert,outcome:"committed",bugRequestId:"rel_fixture-rollback-bug"});
 const receipt=JSON.parse(readFileSync(join(c.home,journal.batch.id+"-receipt.json"),"utf8"));
 assert.equal(receipt.outcome,"rolled_back");assert.deepEqual(receipt.jobs.map(j=>j.jobId),["rel_b1","rel_b2","rel_b3"]);assert.deepEqual(receipt.revert,a.receipts[0].revert);assert.deepEqual(receipt.targets.map(t=>[t.target,t.outcome,t.rollback]),[["tailos","rolled_back","restored"]]);
 assert.ok(!existsSync(join(c.home,"rel_b2.json")));assert.ok(!existsSync(join(c.home,"rel_b3.json")));
 assert.deepEqual(c.solo,["rel_b2","rel_b3"]);assert.deepEqual([...readBatchSolo(c.home)],["rel_b2","rel_b3"]);
 assert.equal(a.calls.filter(x=>x==="rollback:tailos").length,1);
});
test("batch5 an absent setting or a size of 1 is today's release",async()=>{
 for(const batch of [undefined,{max:1,candidates:"members"},{max:1,candidates:[]},{max:3,candidates:[]}]){
  const b=batchJobs([["client/a.js","a"],["client/b.js","b"],["client/c.js","c"],["client/d.js","d"]],{moved:null}),a=batchFake(b.lead),c=batchConfig(b,3);
  delete a.verifyIntegrated;a.verifyIntegrated=async()=>true;
  if(batch)c.batch={...c.batch,...batch,candidates:batch.candidates==="members"?b.members:batch.candidates};else delete c.batch;
  const r=await runRelease(c,a);
  assert.equal(r.outcome,"released");assert.deepEqual(a.calls,["merged","deploy:tailos","finish"]);assert.deepEqual(a.refused,[]);
  const journal=batchJournal(c);
  for(const key of ["batch","batchOps","batchLead","batchReceipt","batchDropped","batchFallback"])assert.ok(!(key in journal),key);
  assert.equal(r.commit,b.lead.commit);assert.deepEqual(batchFiles(c.home),[]);assert.deepEqual(readdirSync(c.home),["rel_b1.json"]);
 }
 assert.deepEqual(releaseBatchPolicy({}),{max:1,importWaitMs:BATCH_IMPORT_WAIT_MS});assert.deepEqual(releaseBatchPolicy({releaseBatch:{}}),{max:1,importWaitMs:BATCH_IMPORT_WAIT_MS});
 assert.deepEqual(releaseBatchPolicy({releaseBatch:{maxJobs:1}}),{max:1,importWaitMs:BATCH_IMPORT_WAIT_MS});assert.deepEqual(releaseBatchPolicy({releaseBatch:{maxJobs:8,importWaitMs:60000}}),{max:8,importWaitMs:60000});
 for(const maxJobs of [0,1.5,"3",9,-1,null,true]){
  assert.throws(()=>releaseBatchPolicy({releaseBatch:{maxJobs}}),/Invalid release batch size/,String(maxJobs));
  // The daemon stops before any command.
  const cwd=mkdtempSync(join(tmpdir(),"release-batch-size-")),log=join(cwd,"calls"),tt=join(cwd,"tt");
  writeFileSync(tt,"#!"+process.execPath+`\nrequire('fs').appendFileSync(${JSON.stringify(log)},'call\\n');process.exit(2);`);chmodSync(tt,0o755);
  await assert.rejects(serveDeployment({version:1,enabled:true,cwd,journalDirectory:cwd,tt,releaseBatch:{maxJobs}},{once:true}),/Invalid release batch size/);
  assert.ok(!existsSync(log));
 }
 for(const releaseBatch of [null,3,[3],"3"])assert.throws(()=>releaseBatchPolicy({releaseBatch}),/Invalid release batch size/);
 for(const importWaitMs of [0,-5,1.5,"60000"])assert.throws(()=>releaseBatchPolicy({releaseBatch:{maxJobs:3,importWaitMs}}),/Invalid release batch import wait/);
});
test("batch6 batch id vector",()=>{
 // The same literal is asserted by TestReleaseBatchIdAndReplay in hub/internal/store.
 assert.equal(batchId("rel_0123456789abcdef",2,"a".repeat(40)),"bat_093efed8fd1877b6");
});
test("batch7 a hub without batch operations releases the lead alone",async()=>{
 const b=three(),a=batchFake(b.lead,{refuse:()=>true}),c=batchConfig(b,3);
 const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");assert.deepEqual(a.calls,["verify","merged","deploy:tailos","finish"]);
 assert.deepEqual(a.refused,["batch-open","batch-open","batch-drop","batch-drop"].slice(0,2),"one open, sent twice, and nothing else");
 const journal=batchJournal(c);
 assert.equal(journal.batch,undefined);assert.deepEqual(journal.batchOps.map(o=>[o.op,o.status]),[["batch-open","refused"]]);
 assert.deepEqual(journal.batchDropped,[{jobId:"rel_b2",reason:"batch-open-refused"}]);
 assert.deepEqual(git(b.f.cwd,"diff","--name-only",b.tip,r.commit).split("\n"),["client/a.js"]);assert.equal(git(b.f.cwd,"rev-parse","refs/heads/tasks-hub"),r.commit);
 assert.deepEqual(batchFiles(c.home),[]);assert.deepEqual(c.solo,[]);assert.ok(!existsSync(join(c.home,"rel_b2.json")));clean(b.f.cwd);
});
// A repository with an approved matrix and Go packages, for real plans.
function matrixRepo(){
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
const race=plan=>plan.checks.find(c=>c.id==="go-race").argv;
// The integrated plan as launchMatrixRun asks for it: the job's plan with the
// integrated commit and the deployer as verifier.
const integratedPlan=(m,job,commit)=>planWithPreservation({...job.plan,commit,verifierAgentId:"agt_fixture",verifierRunId:"run_fixture"},m.f.cwd).plan;
const covers=(...jobs)=>jobs.map(j=>({jobId:j.id,checks:j.plan.checks,matrixDigest:j.plan.matrixDigest}));
// A hub change selects the paired hub and bridge, which need pinned inputs.
const matrixFake=lead=>{const a=batchFake(lead);a.prepare=async t=>["hub","bridge"].includes(t)?{...PAIRED}:{release:"fixture-release",artifactSHA256:"b".repeat(64)};return a;};
test("batch8 a docs-only member keeps ./... in the batch plan",async()=>{
 const m=matrixRepo(),lead=m.accept(1,"hub/internal/store/store.go","package store\n\nvar Lead = 1\n"),docs=m.accept(2,"hub/cmd/tt/testdata/README.md","new\n"),code=m.accept(3,"hub/internal/api/api.go","package api\n\nvar Code = 1\n");m.settle();
 assert.deepEqual(race(lead.plan).slice(-1),["./internal/store"]);assert.deepEqual(race(docs.plan).slice(-1),["./..."]);assert.deepEqual(race(code.plan).slice(-1),["./internal/api"]);
 for(const member of [docs,code])assert.equal(batchMismatch(lead,member),"");
 const merged=batchAcceptedChecks(lead,[docs,code]);
 assert.deepEqual(merged.joined,["rel_m2","rel_m3"]);assert.deepEqual(merged.dropped,[]);assert.deepEqual(merged.checks.find(c=>c.id==="go-race").argv.slice(-1),["./..."]);assert.equal(merged.checksDigest,matrixDigest(merged.checks));
 assert.deepEqual(batchAcceptedChecks(lead,[code]).checks.find(c=>c.id==="go-race").argv.slice(-2),["./internal/api","./internal/store"]);
 // Through runRelease: the job handed to the matrix carries the merged plan.
 const b={f:m.f,lead,members:[docs,code]},a=matrixFake(lead),c=batchConfig(b,3);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");
 const handed=a.verifies[0];
 assert.deepEqual(race(handed.plan).slice(-1),["./..."]);assert.equal(handed.plan.checksDigest,matrixDigest(handed.plan.checks));assert.equal(handed.plan.commit,lead.commit);
 assert.deepEqual(handed.batchCovers.map(x=>x.jobId),["rel_m1",batchJournal(c).batch.id,"rel_m2","rel_m3"]);
 // The real plan from that context covers every job; from the lead's own plan it does not.
 const plan=integratedPlan(m,handed,r.commit);
 assert.deepEqual(race(plan).slice(-1),["./..."]);assert.equal(batchPlanCovers(plan,handed.batchCovers),null);assert.equal(batchPlanCovers(plan,covers(lead,docs,code)),null);
 const leadOnly=integratedPlan(m,lead,r.commit);
 assert.ok(!race(leadOnly).includes("./..."));assert.deepEqual(batchPlanCovers(leadOnly,covers(lead,docs,code)),{jobId:"rel_m2",checkId:"go-race"});assert.equal(batchPlanCovers(leadOnly,covers(lead,code)),null,"the integrated diff already names the code member's package");
 // A check that differs in anything but go-race packages cannot share the run.
 const other=structuredClone(code);other.plan.checks.find(c=>c.id==="go-vet").argv.push("-x");
 assert.equal(batchMismatch(lead,other),"batch-plan-mismatch");assert.deepEqual(batchAcceptedChecks(lead,[other,docs]).dropped,[{jobId:"rel_m3",reason:"batch-plan-mismatch"}]);
 const flags=structuredClone(code);flags.plan.checks.find(c=>c.id==="go-race").argv.splice(2,0,"-count=1");assert.equal(batchMismatch(lead,flags),"batch-plan-mismatch");
 const extra=structuredClone(code);extra.plan.checks.push({id:"npm-unit",argv:["npm","test"],cwd:".",environment:{}});assert.equal(batchMismatch(lead,extra),"batch-plan-mismatch");
});
test("batch9 a job with another base is not tried and releases alone",async()=>{
 const m=matrixRepo(),lead=m.accept(1,"hub/internal/store/store.go","package store\n\nvar Lead = 1\n");
 git(m.f.cwd,"checkout","--quiet","tasks-hub");writeFileSync(join(m.f.cwd,"docs/later.md"),"later\n");git(m.f.cwd,"add",".");git(m.f.cwd,"commit","-qm","later tip");const later=git(m.f.cwd,"rev-parse","HEAD");
 const moved=m.accept(2,"hub/internal/api/api.go","package api\n\nvar Moved = 1\n",{base:later}),same=m.accept(3,"hub/cmd/tt/main.go","package main\n\nfunc main() { _ = 1 }\n");m.settle();
 const summaries=[lead,moved,same].map((j,i)=>({...j,summary:true,rowId:i+1}));
 assert.deepEqual(batchCandidates(summaries,lead).map(j=>j.id),["rel_m3"]);assert.equal(batchMismatch(lead,moved),"batch-ineligible");
 assert.equal(batchMismatch(lead,{...same,plan:{...same.plan,matrixApprovalMessageSeq:10}}),"batch-other-matrix");assert.equal(batchMismatch(lead,{...same,plan:{...same.plan,matrixDigest:"e".repeat(64)}}),"batch-other-matrix");
 for(const change of [{state:"claimed"},{agentId:"agt_other"},{pauseGeneration:2},{rowId:0}])assert.deepEqual(batchCandidates(summaries.map(j=>j.id==="rel_m3"?{...j,...change}:j),lead),[],JSON.stringify(change));
 assert.deepEqual(batchCandidates(summaries,lead,new Set(["rel_m3"])),[]);assert.deepEqual(batchCandidates(summaries,lead,new Set(["rel_m1"])),[],"a job marked to release alone leads no batch");
 // Even handed to the runner, the other-base job is never picked or declared.
 const a=matrixFake(lead),c=batchConfig({f:m.f,lead,members:[moved]},3);
 const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");assert.deepEqual(a.calls,["verify","merged","deploy:hub","deploy:mini","finish"]);assert.deepEqual(a.refused,[]);
 assert.deepEqual(batchJournal(c).batchDropped,[{jobId:"rel_m2",reason:"batch-ineligible"}]);assert.equal(batchJournal(c).batch,undefined);
 assert.deepEqual(git(m.f.cwd,"diff","--name-only",later,r.commit).split("\n"),["hub/internal/store/store.go"]);
 // It then releases alone, in its turn.
 const next={...moved,state:"claimed",generation:2,agentId:"agt_fixture",runId:"run_fixture"},a2=matrixFake(next),c2=batchConfig({f:m.f,lead:next,members:[]},3);
 const r2=await runRelease(c2,a2);assert.equal(r2.outcome,"released");assert.ok(!a2.calls.some(x=>x.startsWith("batch-")));
});
test("batch10 an uncovered plan falls back before any matrix run",async()=>{
 // The host adapter proves coverage after the plan is written and before the run starts.
 const m=matrixRepo(),lead=m.accept(1,"hub/internal/store/store.go","package store\n\nvar Lead = 1\n"),docs=m.accept(2,"hub/cmd/tt/testdata/README.md","new\n");m.settle();
 const home=mkdtempSync(join(tmpdir(),"release-batch-host-"));ignorePrerequisites(m.f.cwd);placePrerequisites(m.f.cwd);
 const picked=await integrateBatch(m.f.cwd,lead,[docs],3,{open:async()=>"bat_0000000000000000",add:async()=>true});
 assert.deepEqual(picked.batch.jobs.map(j=>j.jobId),["rel_m1","rel_m2"]);
 const host=new HostAdapter({cwd:m.f.cwd,journalDirectory:home},{...lead});const started=[];let planned=0;
 host.processStartTime=()=>null;host.startMatrixRun=(argv,dir)=>{started.push(dir);return exitedPid();};
 host.command=argv=>{
  if(argv[1]==="deployment"&&argv[2]==="get")return JSON.stringify(lead);
  if(argv[1]==="scripts/verify-matrix.mjs"&&argv[2]==="plan"){planned++;writeFileSync(argv[4],JSON.stringify(planWithPreservation(JSON.parse(readFileSync(argv[3],"utf8")),m.f.cwd).plan));}
  return "";
 };
 const leadOnly={...lead,integratedCommit:picked.integrated,batchCovers:covers(lead,docs)};
 await assert.rejects(host.verifyIntegrated(leadOnly),e=>failureReason(e)==="batch-plan-uncovered");
 assert.equal(planned,1);assert.deepEqual(started,[]);assert.ok(!existsSync(join(home,"rel_m1-integrated-verification",picked.integrated+"-r0","run.json")),"no run was recorded");
 // With the merged plan the same proof passes and the run starts.
 const merged=batchAcceptedChecks(lead,[docs]);
 assert.equal(await host.verifyIntegrated({...leadOnly,plan:{...lead.plan,checks:merged.checks,checksDigest:merged.checksDigest}}),false);
 // The first directory holds the refused plan; a new attempt directory is not
 // needed because no run was recorded there.
 assert.equal(started.length,1);
 // Without batchCovers nothing is checked: a single release is untouched.
 git(m.f.cwd,"checkout","--quiet","--detach","tasks-hub");
 // Through runRelease: the fallback is recorded by name and the lead goes on alone.
 const b={f:m.f,lead,members:[docs]},a=matrixFake(lead),c=batchConfig(b,3);
 a.verifyIntegrated=async j=>{a.calls.push("verify");a.verifies.push(j);if(j.batchCovers)throw releaseError("batch-plan-uncovered");return true;};
 const r=await runRelease(c,a);
 // The lead alone is a fast-forward of tasks-hub here, so it needs no second matrix.
 assert.equal(r.outcome,"released");assert.deepEqual(a.calls,["batch-open","batch-add:tqe_m1","batch-add:tqe_m2","verify","batch-drop","merged","deploy:hub","deploy:mini","finish"]);assert.equal(r.commit,lead.commit);
 assert.equal(batchJournal(c).batchFallback.reason,"batch-plan-uncovered");assert.equal(batchJournal(c).batchFallback.detail,undefined);assert.deepEqual(c.solo,["rel_m2"]);
 assert.equal(a.verifies.length,1);assert.deepEqual(git(m.f.cwd,"diff","--name-only","refs/heads/tasks-hub~1","refs/heads/tasks-hub").split("\n"),["hub/internal/store/store.go"]);
});
test("batch11 an import not saved in time falls back, and continues if it was saved",async()=>{
 // Not saved: after the wait the batch is dropped and the lead goes on alone.
 const b=three();let clock=1000;const a=batchFake(b.lead),c={...batchConfig(b,3,{importWaitMs:60000}),now:()=>clock};
 let saved=false;a.verifyIntegrated=async j=>{a.calls.push("verify");a.verifies.push(j);if(!j.batchCovers&&!state().batch)return true;a.matrixImportRequested=true;return saved;};
 const state=()=>batchJournal(c);
 assert.deepEqual(await runRelease(c,a),{jobId:"rel_b1",outcome:"waiting_matrix"});
 assert.equal(state().batchImportRequestedAt,1000);assert.equal(state().phase,"waiting_matrix");
 clock+=59999;assert.deepEqual(await runRelease(c,a),{jobId:"rel_b1",outcome:"waiting_matrix"});assert.ok(!a.calls.includes("batch-drop"));assert.equal(state().batchImportRequestedAt,1000,"the first request time is kept");
 clock+=1;const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");assert.deepEqual(a.calls.slice(-5),["batch-drop","verify","merged","deploy:tailos","finish"]);
 assert.deepEqual(state().batchFallback,{batchId:batchId("rel_b1",2,b.tip),reason:"batch-import-timeout",members:["rel_b2","rel_b3"],status:"done"});assert.deepEqual(c.solo,["rel_b2","rel_b3"]);
 assert.deepEqual(git(b.f.cwd,"diff","--name-only",b.tip,r.commit).split("\n"),["client/a.js"]);
 // Saved just as the wait ran out: the hub refuses the drop and the batch continues.
 const b2=three();clock=1000;const a2=batchFake(b2.lead,{refuse:op=>op==="batch-drop"}),c2={...batchConfig(b2,3,{importWaitMs:60000}),now:()=>clock};
 let polls=0;a2.verifyIntegrated=async j=>{a2.calls.push("verify");a2.matrixImportRequested=true;return ++polls>2;};
 assert.equal((await runRelease(c2,a2)).outcome,"waiting_matrix");clock+=60000;
 const r2=await runRelease(c2,a2);
 assert.equal(r2.outcome,"released");assert.deepEqual(a2.refused,["batch-drop","batch-drop"]);assert.ok(!a2.calls.includes("batch-drop"));
 const j2=batchJournal(c2);
 assert.equal(j2.batchFallback.status,"refused");assert.deepEqual(j2.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b2","rel_b3"]);assert.equal(r2.commit,j2.batch.jobs.at(-1).to);assert.deepEqual(c2.solo,[]);
 assert.equal(JSON.parse(readFileSync(join(c2.home,j2.batch.id+"-receipt.json"),"utf8")).outcome,"released");
 // Still not saved after a refused drop: the batch waits, with one drop record.
 const b3=three();clock=1000;const a3=batchFake(b3.lead,{refuse:op=>op==="batch-drop"}),c3={...batchConfig(b3,3,{importWaitMs:60000}),now:()=>clock};
 a3.verifyIntegrated=async()=>{a3.matrixImportRequested=true;return false;};
 await runRelease(c3,a3);clock+=60000;
 for(let i=0;i<3;i++)assert.equal((await runRelease(c3,a3)).outcome,"waiting_matrix");
 assert.equal(batchJournal(c3).batchOps.filter(o=>o.op==="batch-drop").length,1);assert.ok(batchJournal(c3).batch);
});
// A hub in a file: a fake tt that keeps the lead's record, replays a saved
// request id, and can refuse a call or lose its response after saving it.
function batchHub(b,{refuse=[],lose={}}={}){
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
test("batch12 lost batch responses replay before any checkout",async()=>{
 // Saved by the hub, response lost once: sent again unchanged and counted once.
 let b=three(),h=batchHub(b,{lose:{"batch-add:tqe_b2":1}}),a=h.adapter();
 let r=await runRelease(h.cfg(),a);
 assert.equal(r.outcome,"released");assert.deepEqual(h.journal().batch.jobs.map(j=>j.jobId),["rel_b1","rel_b2","rel_b3"]);
 let adds=h.batchCalls().filter(l=>l.argv.includes("--entry tqe_b2"));
 assert.equal(adds.length,2);assert.equal(adds[0].argv,adds[1].argv);assert.match(adds[0].argv,/--generation 4 --request-id rel_b1-batch-add-4 --entry tqe_b2 --commit [a-f0-9]{40}$/);
 assert.equal(h.read().executed["batch-add:tqe_b2"],1);assert.equal(h.read().job.generation,6);assert.deepEqual(a.calls,["verify","merged","deploy:tailos","finish"]);
 // Lost twice: the result is unknown, so the run waits with the checkout where it was.
 b=three();h=batchHub(b,{lose:{"batch-add:tqe_b2":2}});a=h.adapter();
 assert.deepEqual(await runRelease(h.cfg(),a),{jobId:"rel_b1",outcome:"waiting_batch"});
 let journal=h.journal();const pending=journal.batchOps.at(-1),head=git(b.f.cwd,"rev-parse","HEAD");
 assert.equal(journal.phase,"batching");assert.deepEqual([pending.op,pending.status,pending.entryId,pending.generation],["batch-add","sending","tqe_b2",4]);assert.equal(head,pending.commit,"the checkout still holds the picked member");
 assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1"]);assert.deepEqual(a.calls,[]);assert.equal(git(b.f.cwd,"rev-parse","refs/heads/tasks-hub"),b.tip);
 assert.ok(!existsSync(join(tmpdir(),"tailterm-release-locks","none")));
 // The next poll replays that call first, from the same checkout, then finishes with the jobs that joined.
 const before=h.read().log.length;a=h.adapter();r=await runRelease(h.cfg(),a);
 assert.equal(r.outcome,"released");
 const after=h.read().log.slice(before),replay=after.find(l=>/^deployment batch-/.test(l.argv));
 assert.equal(replay.argv,h.batchCalls().filter(l=>l.argv.includes("--entry tqe_b2"))[0].argv);assert.equal(replay.head,head,"replayed before any checkout");
 assert.equal(h.read().executed["batch-add:tqe_b2"],1);
 journal=h.journal();assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b2"]);assert.equal(r.commit,pending.commit);assert.equal(journal.batch.jobs[1].to,pending.commit);
 assert.ok(!h.batchCalls().some(l=>l.argv.includes("tqe_b3")),"a resumed declaration closes with the jobs that joined");
 assert.deepEqual(a.calls,["verify","merged","deploy:tailos","finish"]);clean(b.f.cwd);
 // Every member refused: the open batch has no member, so it is dropped and the lead goes alone.
 b=three();h=batchHub(b,{refuse:["batch-add:tqe_b2","batch-add:tqe_b3"]});a=h.adapter();
 r=await runRelease(h.cfg(),a);
 assert.equal(r.outcome,"released");
 assert.deepEqual(h.batchCalls().map(l=>l.argv.split(" ")[1]+(/--entry (\S+)/.exec(l.argv)?.[1]?":"+/--entry (\S+)/.exec(l.argv)[1]:"")),["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b2","batch-add:tqe_b3","batch-add:tqe_b3","batch-drop"]);
 journal=h.journal();assert.equal(journal.batch,null);assert.deepEqual(journal.batchDropped,[{jobId:"rel_b2",reason:"batch-add-refused"},{jobId:"rel_b3",reason:"batch-add-refused"}]);
 assert.deepEqual(git(b.f.cwd,"diff","--name-only",b.tip,r.commit).split("\n"),["client/a.js"]);assert.equal(journal.batchReceipt,undefined);
 // An older hub answers every batch call as invalid: one open, then the lead alone.
 b=three();h=batchHub(b,{refuse:["batch-open"]});a=h.adapter();
 r=await runRelease(h.cfg(),a);assert.equal(r.outcome,"released");assert.deepEqual(h.batchCalls().map(l=>l.argv.split(" ")[1]),["batch-open","batch-open"]);assert.equal(h.read().job.generation,2);
 // validateNativeRelease: the three calls keep the claim and move one generation;
 // only batch-drop may come back without the integrated commit.
 const prior={...b.lead,integratedCommit:"c".repeat(40)},next=over=>({...prior,generation:3,...over});
 for(const op of ["batch-open","batch-add","batch-drop"]){
  const after=over=>{const n=next(over);if(op==="batch-drop")delete n.integratedCommit;return n;};
  assert.equal(validateNativeRelease(prior,after(),op,2).generation,3);
  assert.throws(()=>validateNativeRelease({...prior,state:"merged"},after(),op,2),/Invalid native release transition/);
  assert.throws(()=>validateNativeRelease(prior,after({generation:2}),op,2),/Invalid native release response/);
  assert.throws(()=>validateNativeRelease(prior,after({state:"verified"}),op,2),/Invalid native release response/);
  assert.throws(()=>validateNativeRelease(prior,after({runId:"run_other"}),op,2),/run binding mismatch/);
  assert.throws(()=>validateNativeRelease(prior,after({inputsDigest:"d".repeat(64)}),op,2),/input binding mismatch/);
  assert.throws(()=>validateNativeRelease(prior,after({receipt:{version:1}}),op,2),/receipt changed/);
 }
 const cleared=next();delete cleared.integratedCommit;
 assert.equal(validateNativeRelease(prior,cleared,"batch-drop",2),cleared);assert.throws(()=>validateNativeRelease(prior,next(),"batch-drop",2),/integrated commit mismatch/);
 for(const op of ["batch-open","batch-add"]){
  assert.throws(()=>validateNativeRelease(prior,cleared,op,2,["--commit","e".repeat(40)]),/integrated commit mismatch/);
  assert.ok(validateNativeRelease(prior,next(),op,2,["--commit","e".repeat(40)]),"a batch call's --commit is not the lead's integrated commit");
 }
});
test("batch13 a crash after hub settlement materializes the receipts without redeploying",async()=>{
 // The hub saved the finish; the runner stopped before it learned so.
 const stopped=async(check)=>{
  const b=three(),a=batchFake(b.lead),c=batchConfig(b,3);let sent;if(check)a.check=check;
  a.finish=async r=>{sent=structuredClone(r);a.calls.push("finish");throw new Error("connection lost");};
  let result;try{result=await runRelease(c,a);}catch(error){result=error;}
  return {b,a,c,sent,result};
 };
 const settled=(x,journal)=>({...x.b.lead,state:x.sent.outcome,generation:journal.finishGeneration+1,integratedCommit:x.sent.commit,published:true,receipt:x.sent});
 let x=await stopped();
 assert.deepEqual(x.result,{jobId:"rel_b1",outcome:"receipt_pending"});
 let journal=batchJournal(x.c);const id=journal.batch.id,path=join(x.c.home,id+"-receipt.json");
 assert.equal(journal.phase,"receipt_pending");assert.equal(journal.batchReceipt.batchId,id);assert.deepEqual(batchFiles(x.c.home),[],"nothing is written before the hub's settlement is known");assert.ok(!existsSync(join(x.c.home,"rel_b2.json")));
 // A half-written batch receipt and a stale temporary file are replaced.
 writeFileSync(path,'{"batchId":"');writeFileSync(path+".tmp","partial");writeFileSync(join(x.c.home,"rel_b2.json"),'{"jobId":"rel_b2","phase":"refused"}');
 const calls=x.a.calls.length;
 reconcileReceipts({journalDirectory:x.c.home},[settled(x,journal)]);
 assert.equal(batchJournal(x.c).phase,"complete");assert.equal(x.a.calls.length,calls,"no prepare, deploy or second finish");
 const receipt=JSON.parse(readFileSync(path,"utf8"));
 assert.equal(receipt.outcome,"released");assert.deepEqual(receipt.jobs.map(j=>j.jobId),["rel_b1","rel_b2","rel_b3"]);assert.deepEqual(receipt.push,x.sent.push);
 for(const member of ["rel_b2","rel_b3"]){const m=JSON.parse(readFileSync(join(x.c.home,member+".json"),"utf8"));assert.equal(m.batch.id,id);assert.equal(m.phase,"complete");assert.equal(m.receipt.jobId,member);}
 assert.equal(JSON.parse(readFileSync(join(x.c.home,`rel_b2.json.before-${id}`),"utf8")).phase,"refused","an earlier journal of the member is kept");
 // The same bytes on every repeat, from any path.
 const bytes=()=>[id+"-receipt.json","rel_b2.json","rel_b3.json"].map(n=>readFileSync(join(x.c.home,n),"utf8"));const first=bytes();
 materializeBatch(x.c.home,batchJournal(x.c));assert.deepEqual(bytes(),first);
 assert.deepEqual(await runRelease(x.c,x.a),x.sent);assert.deepEqual(bytes(),first);assert.equal(x.a.calls.length,calls);
 // The runner's own retry of a pending finish writes them too.
 x=await stopped();journal=batchJournal(x.c);x.a.finish=async()=>{x.a.calls.push("finish-retry");};
 assert.deepEqual(await runRelease(x.c,x.a),x.sent);
 assert.deepEqual(x.a.calls.slice(-2),["finish","finish-retry"]);assert.equal(x.a.calls.filter(n=>n.startsWith("deploy:")).length,1);
 assert.deepEqual(batchFiles(x.c.home),[journal.batch.id+"-receipt.json"]);assert.ok(existsSync(join(x.c.home,"rel_b3.json")));
 // Rolled back: the batch receipt only, never a member journal.
 x=await stopped(async()=>"identity");
 assert.match(x.result.message,/final receipt pending retry/);journal=batchJournal(x.c);assert.equal(journal.phase,"receipt_pending");assert.equal(x.sent.outcome,"rolled_back");
 const rollbacks=x.a.calls.filter(n=>n==="rollback:tailos").length;
 reconcileReceipts({journalDirectory:x.c.home},[settled(x,journal)]);
 const rolled=JSON.parse(readFileSync(join(x.c.home,journal.batch.id+"-receipt.json"),"utf8"));
 assert.equal(rolled.outcome,"rolled_back");assert.deepEqual(rolled.revert,x.sent.revert);assert.ok(!existsSync(join(x.c.home,"rel_b2.json")));assert.ok(!existsSync(join(x.c.home,"rel_b3.json")));
 assert.equal(x.a.calls.filter(n=>n==="rollback:tailos").length,rollbacks);
 // Nothing is written from a journal without a batch, or without a final outcome.
 assert.equal(materializeBatch(x.c.home,{receipt:x.sent}),null);assert.equal(materializeBatch(x.c.home,{batchReceipt:journal.batchReceipt,receipt:{...x.sent,outcome:"pending"}}),null);
});
// A hub for the daemon: the ledger, claims, fence checks, merged and finish,
// and the batch calls unless it is an older hub.
function daemonHub(b,{batchSupported=true,releaseBatch}={}){
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
const SINGLE_JOB_TRACE=["list --view active","list --view settled","get --job rel_b1","list --view active","list --view active","claim --job rel_b1 --generation 1","check --job rel_b1 --generation 2","check --job rel_b1 --generation 2","check --job rel_b1 --generation 2","check --job rel_b1 --generation 2","merged --job rel_b1 --generation 2","check --job rel_b1 --generation 3","check --job rel_b1 --generation 3","check --job rel_b1 --generation 3","finish --job rel_b1 --generation 3"];
const traceOf=log=>log.filter(l=>l.startsWith("deployment ")).map(l=>l.split(" ").slice(1).filter((w,i,all)=>!["--limit","--snapshot","--request-id","--commit","--file"].includes(w)&&!["--limit","--snapshot","--request-id","--commit","--file"].includes(all[i-1])).join(" "));
test("batch14 daemon traces with batching absent and at 1 against an older hub",async()=>{
 const traces=[];
 for(const releaseBatch of [undefined,{maxJobs:1},{}]){
  const b=batchJobs([["client/a.js","a"],["client/b.js","b"],["client/c.js","c"],["client/d.js","d"]]),h=daemonHub(b,{batchSupported:false,releaseBatch});
  await h.poll();
  const state=h.read();
  assert.equal(state.jobs[0].state,"released");assert.deepEqual(state.jobs.slice(1).map(j=>[j.state,j.generation]),[["verified",1],["verified",1],["verified",1]]);
  assert.deepEqual(traceOf(state.log),SINGLE_JOB_TRACE);assert.ok(!state.log.some(l=>l.startsWith("deployment batch-")));assert.equal(state.log.filter(l=>l.startsWith("deployment get")).length,1,"no extra detail read");
  assert.equal(h.seen.length,1);assert.ok(!("batch" in h.seen[0]),"no batch key reaches runRelease");
  assert.deepEqual(readdirSync(h.home).filter(n=>n.startsWith("bat_")||n.startsWith("batch-")||/^rel_b[234]/.test(n)),[]);assert.ok(!("batch" in JSON.parse(readFileSync(join(h.home,"rel_b1.json"),"utf8"))));
  traces.push(state.log.map(l=>l.replace(/[a-f0-9]{40}/g,"SHA").replace(h.home,"HOME")));
 }
 assert.deepEqual(traces[1],traces[0],"a size of 1 sends exactly what an absent setting sends");assert.deepEqual(traces[2],traces[0]);
 // Enabled against the same older hub: one refused open, then the lead alone.
 const b=batchJobs([["client/a.js","a"],["client/b.js","b"],["client/c.js","c"],["client/d.js","d"]]),h=daemonHub(b,{batchSupported:false,releaseBatch:{maxJobs:3}});
 await h.poll();
 assert.equal(h.read().jobs[0].state,"released");assert.deepEqual(h.read().jobs.slice(1).map(j=>j.state),["verified","verified","verified"]);
 assert.equal(h.read().log.filter(l=>l.startsWith("deployment batch-open")).length,2);assert.equal(h.read().log.filter(l=>l.startsWith("deployment batch-")).length,2);
});
test("batch15 a size of 3 with five eligible jobs batches three and leaves two waiting",async()=>{
 const b=batchJobs([["client/a.js","a"],["client/b.js","b"],["client/c.js","c"],["client/d.js","d"],["client/e.js","e"],["client/f.js","f"]]),h=daemonHub(b,{releaseBatch:{maxJobs:3}});
 await h.poll();
 const state=h.read(),journal=JSON.parse(readFileSync(join(h.home,"rel_b1.json"),"utf8"));
 assert.deepEqual(state.batch,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3"]);
 assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b2","rel_b3"]);assert.equal(state.jobs[0].state,"released");assert.equal(state.jobs[0].receipt.jobId,"rel_b1");
 assert.equal(h.seen[0].batch.max,3);assert.deepEqual(h.seen[0].batch.candidates.map(j=>j.id),["rel_b2","rel_b3","rel_b4","rel_b5"],"twice the open places are read, in queue order");
 assert.deepEqual(state.log.filter(l=>l.startsWith("deployment get")).map(l=>l.split(" ")[3]),["rel_b1","rel_b2","rel_b3","rel_b4","rel_b5"]);
 // The two that did not fit were never declared and wait as they were.
 assert.deepEqual(state.jobs.slice(3).map(j=>[j.id,j.state,j.generation]),[["rel_b4","verified",1],["rel_b5","verified",1],["rel_b6","verified",1]]);
 for(const id of ["rel_b4","rel_b5","rel_b6"])assert.ok(!existsSync(join(h.home,id+".json")));
 const receipt=JSON.parse(readFileSync(join(h.home,journal.batch.id+"-receipt.json"),"utf8"));
 assert.deepEqual(receipt.jobs.map(j=>j.jobId),["rel_b1","rel_b2","rel_b3"]);assert.equal(receipt.outcome,"released");
 assert.deepEqual(git(b.f.cwd,"diff","--name-only",b.tip,"refs/heads/tasks-hub").split("\n"),["client/a.js","client/b.js","client/c.js"]);
 assert.ok(existsSync(join(h.home,"rel_b2.json"))&&existsSync(join(h.home,"rel_b3.json")));assert.deepEqual([...readBatchSolo(h.home)],[]);
});
