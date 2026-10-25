import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,lstatSync,readdirSync,rmSync,symlinkSync,renameSync,utimesSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname} from "node:path";
import {execFileSync,spawn,spawnSync} from "node:child_process";
import {integrateCandidate,publishIntegration,runRelease,liveCheck,runnableJob,HostAdapter,cliRetry,serveDeployment,hostLockNames,retentionPolicy,pruneJournal,rehearsalArgv,failureRehearsal,reconcileHostLocks,revertCommit,moveReleaseRef,tasksHubCheckedOut,releaseError,failureReason,receiptReason,probeDiagnostics,withoutDiagnostics,sameReceipt,validateNativeRelease,pushRelease,pushCauseText,pushGroupState,processStartTime,bootIdentity,startMatch,bootMatch,startedBy,PUSH_REASONS,PUSH_TIMEOUT_MS,PUSH_STOP_GRACE_MS,PUSH_STOP_WAIT_MS,reconcileReceipts,MATRIX_PREREQUISITES,MATRIX_HOST_WAIT_MS,MATRIX_LAUNCH_GRACE_MS,MATRIX_STOP_GRACE_MS,MATRIX_DEADLINE_SLACK_MS,missingPrerequisites,provisionPrerequisites,matrixRunTimeout,matrixPriority,matrixWaitNotice,matrixHeldNotice,fenceWaitNotice,matrixRunUnsettled} from "../scripts/release-runner.mjs";
import {acquireHostLock,readJournal,updateHostState,pidGone,groupGone,RUN_TIMEOUT_GRACE_MS,DEFAULT_HOLDER_CAP_MS} from "../scripts/verify-matrix-host-lock.mjs";
import {planRunTimeout,readPrerequisites,provisionWorktreePrerequisites} from "../scripts/verify-matrix.mjs";
import {createHash} from "node:crypto";
import {renderTeamDelivery} from "../client/team-delivery-view.js";
import {targetsForPaths,selectReleaseTargets} from "../scripts/release-targets.mjs";
import {releaseReply,releaseReplySource,readHostState,git,bufferTT,fixture,change,job,fake,config,miniFixture,hash,stamped,importInputs,hostFixture,manifestFile,reclaimed,otherGeneration,digestMismatch,oldName,HUB_SERVICE,rehearsalFixture,REPO,composePlan,recorded,schemaRelease,journalFixture,copyOf,sweep,C,DIST,SWEEP_AT,tailosJob,fiveReleased,addDist,distFixture,sweepAt,distRecord,addAttempt,EVIDENCE,BULK,YOUNG,attemptRecord,bulkRecords,remoteHead,goWorktreeFixture,PAIRED,pairedRelease,pairedHost,RUNNER,ignorePrerequisites,placePrerequisites,workedPlan,exitedPid,attemptDir,matrixHost,provisionFixture,until,ended,idle,lockHost,holderEntry,otherHolder,reasonIs,releaseHost,tailosAdapter,MATRIX_A,MATRIX_B,uncoveredReason,changedMatrixHost,matrixChangeRelease,heldRun,three,race,covers,stale,claimed,hubJob,journalAt} from "./release-runner-fixtures.mjs";
// The real entry point reports progress for --once too: one report before the
// first read and one after the last call. This fake tt rejects the command
// (exit 99), so the first report is followed by the one withdraw.
const START=["deployment progress --phase start","deployment progress --request-id progress-withdraw-RUN"],POLL="deployment progress --phase poll";
const callLog=path=>readFileSync(path,"utf8").replace(/progress-withdraw-\S+/g,"progress-withdraw-RUN");
test("buf1 a valid fake tt deployment list above eight MiB parses and polls",t=>{
 const f=bufferTT(t,`fs.writeSync(1,JSON.stringify(releaseReply([{id:'rel_fixture',state:'refused',padding:'x'.repeat(9*1024*1024)}],args)));`);
 const adapter=new HostAdapter({cwd:f.cwd,tt:f.tt},{}),raw=adapter.command([f.tt,"deployment","list","--view","settled"]);
 assert.ok(Buffer.byteLength(raw)>8*1024*1024);const jobs=JSON.parse(raw);
 assert.equal(jobs.version,1);assert.equal(jobs.jobs.length,1);assert.equal(jobs.jobs[0].padding.length,9*1024*1024);
 const poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");assert.equal(poll.stderr,"");
 assert.equal(callLog(f.log),["deployment list --view settled",...START,"deployment list --view active --limit 200","deployment list --view settled --limit 200 --snapshot "+"a".repeat(64),"deployment list --view active --limit 200 --snapshot "+"a".repeat(64),POLL,"deployment progress --request-id progress-withdraw-RUN",""].join("\n"));
});
test("buf2 real fake tt command overflow names ENOBUFS without payloads or claims",t=>{
 const f=bufferTT(t,`fs.writeSync(2,'SYNTHETIC_PRIVATE_TOKEN');const chunk=Buffer.alloc(1024*1024,120);for(let i=0;i<257;i++)fs.writeSync(2,chunk);`);
 const adapter=new HostAdapter({cwd:f.cwd},{});
 assert.throws(()=>adapter.command([f.tt,"deployment","list"]),e=>e.message==="Host operation failed" && failureReason(e)==="tt ENOBUFS (command buffer overflow)");
 const poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");const diagnostic=poll.stderr;
 assert.match(diagnostic,/Deployment poll held.*ENOBUFS.*command buffer overflow/);assert.ok(diagnostic.length<256);assert.ok(!diagnostic.includes("SYNTHETIC_PRIVATE_TOKEN"));
 const calls=callLog(f.log).split("\n");assert.deepEqual(calls.slice(0,4),["deployment list",...START,"deployment list --view active --limit 200"]);
 assert.match(calls[4],/^send --kind notice --subject A deployer CLI call failed --text Deployer CLI call failed: tt deployment list ENOBUFS \(command buffer overflow\)\. /);assert.ok(!calls[4].includes("SYNTHETIC_PRIVATE_TOKEN"));assert.deepEqual(calls.slice(5),[POLL,"deployment progress --request-id progress-withdraw-RUN",""]);
});
test("buf3 fake tt errors and invalid JSON keep bounded non-secret poll diagnostics",t=>{
 for(const body of [`fs.writeSync(1,'SYNTHETIC_PRIVATE_TOKEN');fs.writeSync(2,'SYNTHETIC_PRIVATE_TOKEN');process.exit(2);`,`fs.writeSync(1,'SYNTHETIC_PRIVATE_TOKEN');`]){
  const f=bufferTT(t,body),poll=f.poll();assert.equal(poll.status,0);assert.equal(poll.stdout,"");
  const diagnostic=poll.stderr;assert.match(diagnostic,/Deployment poll held/);assert.ok(diagnostic.length<256);assert.ok(!diagnostic.includes("SYNTHETIC_PRIVATE_TOKEN"));
  assert.match(diagnostic,body.includes("process.exit")?/tt deployment list exit 2/:/unclassified/);const calls=callLog(f.log).split("\n");assert.deepEqual(calls.slice(0,3),[...START,"deployment list --view active --limit 200"]);
  if(body.includes("process.exit")){assert.match(calls[3],/^send --kind notice --subject A deployer CLI call failed --text Deployer CLI call failed: tt deployment list exit 2\. /);assert.ok(!calls[3].includes("SYNTHETIC_PRIVATE_TOKEN"));assert.deepEqual(calls.slice(4),[POLL,"deployment progress --request-id progress-withdraw-RUN",""]);}
  else assert.deepEqual(calls.slice(3),[POLL,"deployment progress --request-id progress-withdraw-RUN",""]);
 }
});
test("fast forward pins exact candidate and ref CAS refuses a race",()=>{const f=fixture();const commit=change(f,"client/a.js","a");const out=integrateCandidate(f.cwd,job(f,commit));assert.equal(out.integrated,commit);git(f.cwd,"update-ref","refs/heads/tasks-hub",commit,f.base);assert.throws(()=>publishIntegration(f.cwd,commit,f.base),/race/);});
test("full divergent series is cherry picked and conflict leaves ref unchanged",()=>{const f=fixture();change(f,"client/a.js","a");const commit=change(f,"client/b.js","b");git(f.cwd,"checkout","tasks-hub");change(f,"client/c.js","c");const prior=git(f.cwd,"rev-parse","HEAD");const out=integrateCandidate(f.cwd,job(f,commit));assert.notEqual(out.integrated,commit);assert.equal(readFileSync(join(f.cwd,"client/b.js"),"utf8"),"b");assert.equal(git(f.cwd,"rev-parse","tasks-hub"),prior);
const g=fixture();const conflicting=change(g,"client/base.js","candidate");git(g.cwd,"checkout","tasks-hub");change(g,"client/base.js","release");assert.throws(()=>integrateCandidate(g.cwd,job(g,conflicting)),/integration-conflict/);assert.equal(git(g.cwd,"status","--porcelain"),"");});
test("mismatched verified SHA is refused",()=>{const f=fixture();const commit=change(f,"client/a.js","a");const j=job(f,commit);j.plan.commit=f.base;assert.throws(()=>integrateCandidate(f.cwd,j),/binding/);});
test("target selection handles rename/delete and rejects unknown paths",()=>{assert.deepEqual(targetsForPaths(["hub/internal/api/releases.go"]),["hub","bridge","mini"]);assert.deepEqual(targetsForPaths(["docs/release.md"]),[]);assert.throws(()=>targetsForPaths(["unknown.xyz"]),/Unknown/);const f=fixture();const commit=change(f,"client/a.js","a");git(f.cwd,"mv","client/base.js","client/moved.js");git(f.cwd,"commit","-m","rename");assert.deepEqual(selectReleaseTargets(f.cwd,Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,f.base])),git(f.cwd,"rev-parse","HEAD")),["tailos"]);});
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
  writeFileSync(manifest,"{}");assert.throws(()=>adapter.jobInputs(commit),digestMismatch);
 }
});
test("the runner reads the manifest the hub bound for this attempt among those of earlier generations",()=>{
 const {commit,adapter,dir,bind}=reclaimed(),first=manifestFile(adapter,commit,"rel_sel-g3-inputs.json",{generation:3,attempt:0}),second=manifestFile(adapter,commit,"rel_sel-g8-inputs.json",{generation:8,attempt:1});
 manifestFile(adapter,commit,"rel_selx-g8-inputs.json",{generation:8,attempt:1});bind(second);
 const input=adapter.jobInputs(commit);assert.deepEqual([input.generation,input.attempt],[8,1]);assert.deepEqual(input,JSON.parse(second.raw));
 assert.equal(readFileSync(first.path,"utf8"),first.raw,"the earlier attempt's manifest is only read");assert.deepEqual(readdirSync(dir).sort(),["rel_sel-g3-inputs.json","rel_sel-g8-inputs.json","rel_selx-g8-inputs.json"]);
 // A file under the name without a generation is never used as the manifest:
 // the generation-named file is checked and read whatever that file holds.
 writeFileSync(join(dir,"rel_sel-inputs.json"),second.raw);assert.deepEqual(adapter.jobInputs(commit),JSON.parse(second.raw));
 writeFileSync(join(dir,"rel_sel-inputs.json"),first.raw);assert.deepEqual(adapter.jobInputs(commit),JSON.parse(second.raw),"an earlier attempt's copy under that name does not matter");
 assert.throws(()=>adapter.jobInputs("e".repeat(40)),e=>failureReason(e)==="Handler input digest binding required");
});
test("the runner's inputs request names the manifest and request id of the generation it read",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"inputs-request-")),calls=[],commit="b".repeat(40);
 const adapter=new HostAdapter({cwd,journalDirectory:cwd,tt:"tt"},{id:"rel_fixture",commit:"a".repeat(40),itemId:"wi_fixture",itemRevision:2,orderMessageSeq:15262,generation:3});
 let generation=8;adapter.command=argv=>{calls.push(argv);if(argv[2]==="handler")return JSON.stringify({id:"agt_0123456789abcdef",name:"db-handler-sol61"});if(argv[2]==="get")return JSON.stringify({...adapter.job,state:"claimed",generation});return "";};
 assert.equal(await adapter.verifyInputs(commit),false);assert.equal(await adapter.verifyInputs(commit),false);generation=11;assert.equal(await adapter.verifyInputs(commit),false);
 const sends=calls.filter(a=>a[1]==="send"),field=(s,name)=>s[s.indexOf(name)+1];
 assert.deepEqual(sends.map(s=>field(s,"--request-id")),[`rel_fixture-g8-inputs-${commit}`,`rel_fixture-g8-inputs-${commit}`,`rel_fixture-g11-inputs-${commit}`],"a poll at an unchanged generation repeats its request id");
 assert.ok(field(sends[0],"--ask").includes(`Prepare the private manifest ${join(cwd,"rel_fixture-g8-inputs.json")} for exact job rel_fixture `));assert.ok(field(sends[2],"--ask").includes(join(cwd,"rel_fixture-g11-inputs.json")));
});
test("a manifest of another generation or attempt is refused with a named reason",()=>{
 // Only an earlier attempt's manifest exists, and none hashes to the bound digest.
 let x=reclaimed();manifestFile(x.adapter,x.commit,"rel_sel-g3-inputs.json",{generation:3,attempt:0});x.adapter.job.inputsDigest="e".repeat(64);
 assert.throws(()=>x.adapter.jobInputs(x.commit),digestMismatch);
 // The bound manifest's bytes changed after the import: the digest is what is refused.
 x=reclaimed();const bound=manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:8,attempt:1});x.bind(bound);assert.deepEqual(x.adapter.jobInputs(x.commit),JSON.parse(bound.raw));
 writeFileSync(bound.path,JSON.stringify({...JSON.parse(bound.raw),targets:{x:1}}));assert.throws(()=>x.adapter.jobInputs(x.commit),digestMismatch);
 // The file named for generation 8 records generation 3.
 x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:3,attempt:1}));assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration);
 // The recorded generation is not below the job's: the import always moves the job on.
 for(const g of [9,12]){x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,`rel_sel-g${g}-inputs.json`,{generation:g,attempt:1}));assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration,"generation "+g);}
 // The recorded attempt is not this one.
 for(const attempt of [0,2,undefined]){x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:8,attempt}));assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration,"attempt "+attempt);}
 // With no manifest of any generation the binding itself is what is missing.
 x=reclaimed();x.adapter.job.inputsDigest="e".repeat(64);assert.throws(()=>x.adapter.jobInputs(x.commit),e=>failureReason(e)==="Handler input digest binding required");
});
test("a manifest under the name without a generation is refused with its own reason",()=>{
 let x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-inputs.json",{}));assert.throws(()=>x.adapter.jobInputs(x.commit),oldName);
 // The name is what is refused, whatever the file records.
 x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-inputs.json",{generation:8,attempt:1}));assert.throws(()=>x.adapter.jobInputs(x.commit),oldName);
 // A generation-named file beside it that does not hash to the digest changes nothing.
 x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-inputs.json",{}));manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:8,attempt:1});assert.throws(()=>x.adapter.jobInputs(x.commit),oldName);
 // Another digest: not the bound manifest, with or without a later attempt's file beside it.
 x=reclaimed();manifestFile(x.adapter,x.commit,"rel_sel-inputs.json",{});x.adapter.job.inputsDigest="e".repeat(64);
 assert.throws(()=>x.adapter.jobInputs(x.commit),e=>failureReason(e)==="Handler input digest binding required");
 manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:8,attempt:1});assert.throws(()=>x.adapter.jobInputs(x.commit),digestMismatch);
});
test("b1 schema rehearsal builds the exact candidate binary for the Linux container and rejects stale job backups",async()=>{
 const {f,home,config}=hostFixture();const commit=change(f,"hub/internal/store/migrate.go","candidate schema"),id="rel_schema";
 config.targets.hub={migrationBinary:join(home,"stale-migration"),schemaChanged:false};const adapter=new HostAdapter(config,{...job(f,commit),id});
 const backup=join(home,id+"-g1-backup"),pin=join(home,"preflight.json"),plan=join(home,"plan.json");writeFileSync(backup,"backup");writeFileSync(pin,"{}");
 const release=id+"-"+commit.slice(0,12)+"-hub";writeFileSync(plan,JSON.stringify({backupDestination:backup,deployment:{releaseName:release,targets:["hub"],binaryDestination:`/mnt/deepfreeze/tailterm-hub/releases/${release}/tailterm-hub`}}));
 const input={release,backupJobId:id,backup,backupCopy:backup,backupSHA256:hash("backup"),preflightReceipt:pin,preflightReceiptSHA256:hash("{}"),planPath:plan,rollbackSafe:true};
 importInputs(adapter,commit,{hub:input});let buildHeads=[],builds=[],composed=[],hostRuns=[],runs=[];
 adapter.command=argv=>{if(argv[1]==="version")return stamped(commit);if(argv.includes("build")){builds.push(argv);buildHeads.push(git(f.cwd,"rev-parse","HEAD"));writeFileSync(argv[argv.indexOf("-o")+1],"candidate migration/binary");return "";}if(argv[0]==="python3"){composed.push(argv.slice(3));return JSON.stringify(HUB_SERVICE);}hostRuns.push(argv);return "";};
 adapter.container=argv=>{if(argv[1]==="run")runs.push(argv);return {status:0,stdout:"",stderr:""};};
 const artifact=await adapter.prepare("hub",commit);assert.equal(artifact.schemaChanged,true);assert.equal(buildHeads.length,2);assert.deepEqual(buildHeads,[commit,commit]);assert.notEqual(artifact.migrationBinary,config.targets.hub.migrationBinary);
 // a3: the migration binary only ever runs in the Linux container.
 const migrationBuild=builds.find(b=>b[b.indexOf("-o")+1]===artifact.migrationBinary);assert.deepEqual(migrationBuild.slice(0,4),["env","CGO_ENABLED=0","GOOS=linux",`GOARCH=${process.arch==="arm64"?"arm64":"amd64"}`]);
 await adapter.rehearse(artifact);assert.deepEqual(composed,[[plan,release]],"the compose comes from this job's plan and release");assert.equal(runs.length,1);assert.ok(runs[0].includes(`${artifact.migrationBinary}:/opt/tailterm-hub:ro`));
 assert.deepEqual(hostRuns,[],"nothing runs the migration on the host");assert.ok(!runs[0].some(x=>x.startsWith(backup+":")),"the imported backup itself is never mounted");assert.equal(readFileSync(backup,"utf8"),"backup");
 importInputs(adapter,commit,{hub:{...input,backupJobId:"rel_previous"}});await assert.rejects(adapter.prepare("hub",commit),/backup identity/);
 // A manifest that records its generation needs that generation's backup.
 for(const stale of [join(home,id+"-backup"),join(home,id+"-g7-backup")]){importInputs(adapter,commit,{hub:{...input,backup:stale}});await assert.rejects(adapter.prepare("hub",commit),/backup identity/,stale);}
});
test("a1 a passing rehearsal runs one container on a private copy, removes its state directory, marks passed and leaves the imported backup unchanged",async()=>{
 const r=rehearsalFixture();assert.equal(await r.adapter.rehearse(r.artifact),true);
 const argv=rehearsalArgv(HUB_SERVICE,{runtime:"container",name:"tt-rehearsal-rel_rehearse",binary:r.binary,stateDir:r.state});
 assert.deepEqual(r.calls,[["container","system","status"],argv]);assert.equal(r.seen[0].copy,"imported backup");
 assert.deepEqual(r.commands.map(c=>[c[0],c[1],...c.slice(3)]),[["python3","-c",r.artifact.planPath,"fixture"]]);
 assert.deepEqual(r.left(),[]);assert.equal(readFileSync(r.backup,"utf8"),"imported backup");
 assert.deepEqual(JSON.parse(readFileSync(r.marker,"utf8")),{version:1,jobId:"rel_rehearse",backupSHA256:hash("imported backup"),startedAt:"2026-10-01T09:00:01.000Z",outcome:"passed",endedAt:"2026-10-01T09:00:02.000Z",copyRemoved:true});
 assert.equal(statSync(r.marker).mode&0o777,0o600);
 // A configured runtime path replaces only the program; no setting selects a host run.
 const p=rehearsalFixture("rel_path");p.adapter.config.rehearsal={runtime:"/opt/homebrew/bin/container"};assert.equal(await p.adapter.rehearse(p.artifact),true);assert.deepEqual(p.calls.map(c=>c.slice(0,2)),[["/opt/homebrew/bin/container","system"],["/opt/homebrew/bin/container","run"]]);
 const bad=rehearsalFixture("rel_badpath");bad.adapter.config.rehearsal={runtime:"sh -c"};await assert.rejects(bad.adapter.rehearse(bad.artifact),/Invalid rehearsal runtime/);assert.deepEqual(bad.calls,[]);
});
test("a2 a failing rehearsal rejects with the container's exit reason, removes its state directory and marks failed",async()=>{
 const r=rehearsalFixture();r.run={status:1,stderr:"2026/10/07 05:12:46 migration rehearsal failed\n"};
 await assert.rejects(r.adapter.rehearse(r.artifact),error=>failureReason(error)==="Rehearsal container exit 1" && error.rehearsal.exit==="exit 1" && error.rehearsal.lines.length===1);
 assert.equal(r.seen.length,1);assert.deepEqual(r.left(),[]);assert.equal(readFileSync(r.backup,"utf8"),"imported backup");
 const marker=JSON.parse(readFileSync(r.marker,"utf8"));assert.equal(marker.outcome,"failed");assert.equal(marker.copyRemoved,true);assert.ok(!readFileSync(r.marker,"utf8").includes("rehearsal failed"));
 // A migration binary changed after the build fails the same way, before any container runs.
 const b=rehearsalFixture("rel_binary");writeFileSync(b.binary,"another binary");
 await assert.rejects(b.adapter.rehearse(b.artifact),/migration binary changed/);assert.equal(b.seen.length,0);assert.deepEqual(b.left(),[]);assert.equal(JSON.parse(readFileSync(b.marker,"utf8")).outcome,"failed");
});
test("a3 the marker, not the copy, refuses a second rehearsal",async()=>{
 for(const fail of [false,true]){
  const r=rehearsalFixture();if(fail)r.run={status:1};
  await r.adapter.rehearse(r.artifact).catch(()=>{});assert.deepEqual(r.left(),[]);const before=readFileSync(r.marker,"utf8"),calls=r.calls.length;
  await assert.rejects(r.adapter.rehearse(r.artifact),/Rehearsal already attempted; inspect prior attempt/);
  assert.equal(r.seen.length,1,"the migration is not run again");assert.equal(r.calls.length,calls,"no container call follows the marker");assert.deepEqual(r.left(),[]);assert.equal(readFileSync(r.marker,"utf8"),before);
 }
 // A run stopped mid-rehearsal leaves a started marker, which refuses too and keeps the copy for inspection.
 const stopped=rehearsalFixture("rel_stopped");writeFileSync(stopped.marker,JSON.stringify({version:1,jobId:"rel_stopped",outcome:"started"}));mkdirSync(stopped.state);writeFileSync(stopped.copy,"half migrated");
 await assert.rejects(stopped.adapter.rehearse(stopped.artifact),/already attempted/);assert.equal(stopped.calls.length,0);assert.equal(readFileSync(stopped.copy,"utf8"),"half migrated");
 // A state directory with no marker does not refuse: it and its sidecars are replaced by a fresh copy.
 const stale=rehearsalFixture("rel_stale");mkdirSync(stale.state);writeFileSync(stale.copy,"stale copy");writeFileSync(stale.copy+"-wal","stale wal");
 assert.equal(await stale.adapter.rehearse(stale.artifact),true);assert.deepEqual(stale.seen.map(c=>[c.copy,c.wal]),[["imported backup",false]]);
 assert.deepEqual(stale.left(),[]);assert.equal(JSON.parse(readFileSync(stale.marker,"utf8")).outcome,"passed");
});
test("c1 the argv built from the real hub_compose carries every production constraint and no production path",()=>{
 const BASE="/mnt/deepfreeze/tailterm-hub",adapter=new HostAdapter({cwd:REPO},{id:"rel_compose"}),where={runtime:"container",name:"tt-rehearsal-rel_compose",binary:"/journal/rel_compose-migration",stateDir:"/journal/copy.state"};
 const plain=adapter.rehearsalSpec(composePlan()),full=adapter.rehearsalSpec(composePlan({typesafeKeyPath:BASE+"/typesafe-key",discordTokenPath:BASE+"/discord-token",bridgeTokenPath:BASE+"/bridge-token",bridgeBinaryDestination:BASE+"/releases/rel_compose-fixture/tailterm-discord",bridgeStateDirectory:BASE+"/bridge-state",discordGuildId:"1",discordApplicationId:"2",discordOwnerIds:"3",tailosUrl:"https://tailos.example.invalid"}));
 assert.deepEqual(plain,{...HUB_SERVICE,volumes:[`${BASE}/releases/rel_compose-fixture/tailterm-hub:/opt/tailterm-hub:ro`,...HUB_SERVICE.volumes.slice(1)]},"the fixture service used above is what the real function composes");
 assert.equal(full.volumes.length,5);assert.equal(Object.keys(full.environment).length,7);
 for(const service of [plain,full]){
  const argv=rehearsalArgv(service,where),env=argv.flatMap((x,i)=>x==="-e"?[argv[i+1]]:[]),mounts=argv.flatMap((x,i)=>x==="-v"?[argv[i+1]]:[]),pair=flag=>argv[argv.indexOf(flag)+1];
  assert.deepEqual(argv.slice(0,7),["container","run","--rm","--progress","none","--name","tt-rehearsal-rel_compose"]);
  assert.ok(argv.includes("--read-only"));assert.equal(pair("--user"),"950:950");assert.equal(pair("--cap-drop"),"ALL");assert.equal(pair("--memory"),"512M");assert.equal(pair("--cpus"),"1");assert.equal(pair("--workdir"),"/");
  assert.deepEqual(env,Object.entries(service.environment).map(([k,v])=>`${k}=${v}`).sort());assert.ok(env.includes("SQLITE_TMPDIR=/state"));
  assert.deepEqual(mounts,["/journal/rel_compose-migration:/opt/tailterm-hub:ro","/journal/copy.state:/state"]);
  assert.deepEqual(argv.slice(-5),["--entrypoint","/opt/tailterm-hub",service.image,"--migrate-only","/state/hub.sqlite"]);assert.equal(service.image,"gcr.io/distroless/static-debian12:nonroot");
  assert.ok(!argv.some(x=>x.includes(BASE)),"no production path, token or key path is an argument");assert.ok(argv.every(x=>!/[\r\n\0]/.test(x)));
 }
 // A plan the candidate's own validation refuses never yields a service.
 const refused=composePlan({tokenPath:"/elsewhere/hub-token"});assert.throws(()=>adapter.rehearsalSpec(refused),e=>failureReason(e)==="python3 exit 1");
 assert.throws(()=>adapter.rehearsalSpec({release:"x"}),/Rehearsal plan required/);
});
test("c4 a composed service the runner does not fully understand is refused with no run and no marker",async()=>{
 const without=key=>{const {[key]:dropped,...rest}=HUB_SERVICE;return rest;},vol=HUB_SERVICE.volumes;
 const shapes={readOnlyFalse:{...HUB_SERVICE,read_only:false},capDrop:{...HUB_SERVICE,cap_drop:["NET_RAW"]},noCapDrop:without("cap_drop"),noUser:without("user"),namedUser:{...HUB_SERVICE,user:"nonroot"},noMemory:without("mem_limit"),memoryUnit:{...HUB_SERVICE,mem_limit:"1g"},
  noCpus:without("cpus"),zeroCpus:{...HUB_SERVICE,cpus:"0"},noImage:without("image"),flagImage:{...HUB_SERVICE,image:"--privileged"},noState:{...HUB_SERVICE,volumes:[vol[0],vol[2]]},noBinary:{...HUB_SERVICE,volumes:vol.slice(1)},writableExtra:{...HUB_SERVICE,volumes:[...vol,"/mnt/x:/data"]},
  noEnvironment:without("environment"),listEnvironment:{...HUB_SERVICE,environment:["A=b"]},lineEnvironment:{...HUB_SERVICE,environment:{A:"b\nc"}},shellEntrypoint:{...HUB_SERVICE,entrypoint:["/bin/sh","-c"]},noPrivileges:without("security_opt"),tmpfs:{...HUB_SERVICE,tmpfs:["/tmp"]},notObject:"hub",list:[HUB_SERVICE]};
 for(const [label,service] of Object.entries(shapes)){
  const r=rehearsalFixture("rel_"+label.toLowerCase());r.service=service;
  await assert.rejects(r.adapter.rehearse(r.artifact),e=>failureReason(e)==="Rehearsal compose unsupported",label);
  assert.deepEqual(r.calls,[["container","system","status"]],label);assert.equal(existsSync(r.marker),false,label);assert.deepEqual(r.left(),[],label);
 }
 // Output that is not a service at all refuses the same way.
 const r=rehearsalFixture("rel_garbage");r.adapter.command=()=>"not json";await assert.rejects(r.adapter.rehearse(r.artifact),/Rehearsal compose unsupported/);assert.equal(existsSync(r.marker),false);
 assert.equal(rehearsalArgv({...HUB_SERVICE,cpus:1.5,mem_limit:"256m"},{runtime:"container",name:"n",binary:"/b",stateDir:"/s"}).join(" ").includes("--memory 256M --cpus 2"),true);
});
test("c5 a failed container run stops the release before any deploy and journals its exit reason and redacted last lines",async()=>{
 const s=schemaRelease("rel_fixture"),secret="abc123SECRETVALUE9876",long="x ".repeat(400);
 s.e.run={status:1,stdout:"stdout noise\n",stderr:[...Array.from({length:43},(_,i)=>"line "+(i+1)),long,"auth token="+secret+" rejected","open /run/hub-token: no such file","2026/10/07 05:12:46 migration rehearsal failed"].join("\n")+"\n"};
 let thrown;await assert.rejects(runRelease(s.c,s.a),e=>{thrown=e;return true;});
 const lines=[...Array.from({length:36},(_,i)=>"line "+(i+8)),long.slice(0,300),"auth [redacted] rejected","open [redacted]: no such file","2026/10/07 05:12:46 migration rehearsal failed"];
 assert.deepEqual(s.journal().failure,{step:"rehearse",target:"hub",reason:"Rehearsal container exit 1",rehearsal:{runtime:"container",image:"gcr.io/distroless/static-debian12:nonroot",exit:"exit 1",lines}});assert.equal(lines.length,40);
 assert.ok(!s.a.calls.some(x=>x.startsWith("deploy:")));
 assert.ok(!readFileSync(s.c.journalPath,"utf8").includes(secret));
 // Nothing the runner hands to its adapter (notices, receipts) or throws carries an output line.
 const out=s.said.join("\n")+String(thrown?.message);for(const text of ["migration rehearsal failed","line 30","no such file",secret,"stdout noise"])assert.ok(!out.includes(text),text);
 assert.deepEqual(s.e.left(),[]);const marker=readFileSync(s.e.marker,"utf8");assert.equal(JSON.parse(marker).outcome,"failed");assert.ok(!marker.includes("rehearsal failed") && !marker.includes("line 30"));
 assert.deepEqual(s.e.calls.map(c=>c[1]),["system","run"]);
});
test("c6 a signal, a timeout and an out-of-memory kill are named, and a malformed rehearsal record is dropped from the journal",async()=>{
 const name="tt-rehearsal-rel_how",removal=["container","delete","-f",name];
 for(const [run,how,removed] of [[{status:null,signal:"SIGKILL"},"signal SIGKILL",true],[{status:null,signal:"SIGTERM",error:Object.assign(new Error("spawnSync container ETIMEDOUT"),{code:"ETIMEDOUT"})},"timeout",true],[{status:137},"exit 137",false],[{status:null,error:Object.assign(new Error("spawn EACCES"),{code:"EACCES"})},"not started",true]]){
  const r=rehearsalFixture("rel_how");r.run=run;
  await assert.rejects(r.adapter.rehearse(r.artifact),e=>failureReason(e)==="Rehearsal container "+how && failureRehearsal(e).exit===how && failureRehearsal(e).lines.length===0,how);
  // Only a run that did not end by itself may have left its container behind.
  assert.deepEqual(r.calls.slice(2),removed?[removal]:[],how);assert.deepEqual(r.left(),[],how);assert.equal(JSON.parse(readFileSync(r.marker,"utf8")).outcome,"failed");
 }
 const good={runtime:"container",image:"gcr.io/distroless/static-debian12:nonroot",exit:"exit 1",lines:["one"]},tagged=rehearsal=>Object.assign(releaseError("Rehearsal container exit 1"),{rehearsal});
 assert.deepEqual(failureRehearsal(tagged({...good,extra:"dropped",lines:["one"]})),good);
 const malformed=[{...good,lines:Array(41).fill("x")},{...good,lines:["x".repeat(301)]},{...good,lines:[7]},{...good,lines:"one"},{...good,exit:"exit 1; rm"},{...good,exit:"killed"},{...good,image:"image with spaces"},{...good,runtime:""},{...good,runtime:"r".repeat(161)},"text",null];
 for(const rehearsal of malformed)assert.equal(failureRehearsal(tagged(rehearsal)),null,JSON.stringify(rehearsal).slice(0,60));
 const s=schemaRelease("rel_fixture");s.a.rehearse=async()=>{throw tagged(malformed[0]);};await assert.rejects(runRelease(s.c,s.a));
 assert.deepEqual(s.journal().failure,{step:"rehearse",target:"hub",reason:"Rehearsal container exit 1"});
});
test("c7 a missing or stopped container runtime stops the release with no host run, no marker and no deploy",async()=>{
 const missing={status:null,error:Object.assign(new Error("spawnSync container ENOENT"),{code:"ENOENT"})};
 for(const [status,reason] of [[missing,"Rehearsal container runtime unavailable"],[{status:1,stderr:"apiserver is not running"},"Rehearsal container runtime not running"],[{status:null,signal:"SIGTERM",error:Object.assign(new Error("ETIMEDOUT"),{code:"ETIMEDOUT"})},"Rehearsal container runtime unavailable"]]){
  const r=rehearsalFixture("rel_runtime");r.status=status;
  await assert.rejects(r.adapter.rehearse(r.artifact),e=>failureReason(e)===reason && !e.rehearsal,reason);
  assert.deepEqual(r.calls,[["container","system","status"]],"no run, and nothing but the runtime check");assert.deepEqual(r.commands,[],"no host command at all, so never the migration binary");
  assert.equal(existsSync(r.marker),false);assert.deepEqual(r.left(),[]);
  // With the runtime back, the same job rehearses: the refusal left no marker.
  r.status={status:0};assert.equal(await r.adapter.rehearse(r.artifact),true);
  const s=schemaRelease("rel_fixture");s.e.status=status;await assert.rejects(runRelease(s.c,s.a));
  assert.deepEqual(s.journal().failure,{step:"rehearse",target:"hub",reason});assert.ok(!s.a.calls.some(x=>x.startsWith("deploy:")));
  assert.ok(!s.e.calls.some(c=>c[1]==="run") && !s.e.commands.some(c=>c[0]===s.e.binary));assert.equal(existsSync(s.e.marker),false);
 }
});
test("c9 retention removes a terminal job's rehearsal state directory and nothing else in it",()=>{
 const jobs=[{id:"rel_done",state:"refused"},{id:"rel_live",state:"blocked"},{id:"rel_link",state:"refused"},{id:"rel_swap",state:"refused"}],state=id=>copyOf(id)+".rehearsal-"+id+".state";
 const j=journalFixture({[copyOf("rel_done")+".rehearsal-rel_done"]:7,[copyOf("rel_done")+".rehearsal-rel_done.json"]:3,"outside.sqlite":9});
 for(const id of ["rel_done","rel_live","rel_link"]){mkdirSync(join(j.dir,state(id)));for(const [name,bytes] of [["hub.sqlite",50],["hub.sqlite-wal",5],["hub.sqlite-shm",4],["etilqs_temp",2]])writeFileSync(join(j.dir,state(id),name),"x".repeat(bytes));}
 // A link and a directory inside a state directory are left, and so is the directory that still holds them.
 symlinkSync(join(j.dir,"outside.sqlite"),join(j.dir,state("rel_link"),"link"));mkdirSync(join(j.dir,state("rel_link"),"nested"));writeFileSync(join(j.dir,state("rel_link"),"nested","kept"),"kept");
 // A state directory name that is itself a link is not entered.
 const elsewhere=mkdtempSync(join(tmpdir(),"release-retention-elsewhere-"));writeFileSync(join(elsewhere,"hub.sqlite"),"elsewhere");symlinkSync(elsewhere,join(j.dir,state("rel_swap")));
 const removed=sweep(j,jobs,{releasedBackups:3,backupBudgetBytes:0}),inside=id=>[["etilqs_temp",2],["hub.sqlite",50],["hub.sqlite-shm",4],["hub.sqlite-wal",5]].map(([name,bytes])=>({jobId:id,file:state(id)+"/"+name,kind:"rehearsal",bytes,reason:"terminal"}));
 const expected=[{jobId:"rel_done",file:copyOf("rel_done")+".rehearsal-rel_done",kind:"rehearsal",bytes:7,reason:"terminal"},...inside("rel_done"),...inside("rel_link")].map(r=>({version:1,at:"2026-10-01T09:30:00.000Z",...r}));
 assert.deepEqual(removed,expected);assert.deepEqual(j.lines(),expected);
 assert.deepEqual(j.names(),["outside.sqlite",copyOf("rel_done")+".rehearsal-rel_done.json",state("rel_link"),state("rel_live"),state("rel_swap")].sort());
 assert.deepEqual(readdirSync(join(j.dir,state("rel_link"))).sort(),["link","nested"]);assert.equal(readFileSync(join(j.dir,state("rel_link"),"nested","kept"),"utf8"),"kept");
 assert.equal(readdirSync(join(j.dir,state("rel_live"))).length,4,"a job that is not terminal keeps its state directory");
 assert.equal(readFileSync(join(elsewhere,"hub.sqlite"),"utf8"),"elsewhere");assert.equal(readFileSync(join(j.dir,"outside.sqlite"),"utf8").length,9);
 assert.deepEqual(sweep(j,jobs,{releasedBackups:3,backupBudgetBytes:0}),[],"a second sweep removes nothing");
});
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
test("a7 every removal is recorded once and nothing but listed backup, rehearsal and migration files is touched",t=>{
 const jobs=[{id:"rel_old",state:"released",settledAt:"2026-09-29T12:00:00Z"},{id:"rel_new",state:"released",settledAt:"2026-09-30T12:00:00Z"},{id:"rel_refused",state:"refused"},{id:"../rel_escape",state:"refused"}];
 const others=["rel_old.json","rel_old-inputs.json","rel_old-receipt.json","rel_old-truenas-plan.json","rel_old-migration.tmp","rel_old-mini-before","rel_old-truenas-backup.sqlite.rehearsal-rel_old.json","rel_old-truenas-backup.sqlite.tmp","rel_unlisted-truenas-backup.sqlite","rel_unlisted-truenas-backup.sqlite.rehearsal-rel_unlisted","mini-relay-offset.json"];
 const parent=mkdtempSync(join(tmpdir(),"release-escape-"));t.after(()=>rmSync(parent,{recursive:true,force:true}));
 const j=journalFixture({[copyOf("rel_old")]:30,"rel_old-hub-backup.sqlite":20,[copyOf("rel_old")+".rehearsal-rel_old"]:7,"rel_old-migration":13,[copyOf("rel_new")]:30,"rel_refused-bridge-backup.sqlite":11,...Object.fromEntries(others.map(n=>[n,3]))},parent);
 // Only a regular file directly in the journal directory is a copy: a directory or link of that name stays.
 mkdirSync(join(j.dir,"tailos-dist-fixture"));mkdirSync(join(j.dir,"rel_refused-hub-backup.sqlite"));symlinkSync(join(j.dir,"rel_old.json"),join(j.dir,copyOf("rel_refused")));
 const escape=join(dirname(j.dir),"rel_escape-truenas-backup.sqlite");writeFileSync(escape,"outside");
 try{
  const removed=sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:0}),expected=[
   {jobId:"rel_old",file:copyOf("rel_old")+".rehearsal-rel_old",kind:"rehearsal",bytes:7,reason:"terminal"},{jobId:"rel_old",file:"rel_old-migration",kind:"migration",bytes:13,reason:"terminal"},{jobId:"rel_refused",file:"rel_refused-bridge-backup.sqlite",kind:"backup",bytes:11,reason:"not-released"},
   {jobId:"rel_old",file:copyOf("rel_old"),kind:"backup",bytes:30,reason:"count"},{jobId:"rel_old",file:"rel_old-hub-backup.sqlite",kind:"backup",bytes:20,reason:"count"}].map(r=>({version:1,at:"2026-10-01T09:30:00.000Z",...r}));
  assert.deepEqual(removed,expected);assert.deepEqual(j.lines(),expected);assert.equal(statSync(join(j.dir,"retention.jsonl")).mode&0o777,0o600);
  assert.deepEqual(j.names(),[...others,copyOf("rel_new"),copyOf("rel_refused"),"rel_refused-hub-backup.sqlite","tailos-dist-fixture"].sort());
  assert.equal(readFileSync(escape,"utf8"),"outside");
  assert.deepEqual(sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:0}),[],"a second sweep removes nothing");assert.deepEqual(j.lines(),expected);
 }finally{rmSync(escape,{force:true});}
});
test("m3 a terminal job loses its migration binary whatever its state and the backup policy, and a job that is not terminal keeps it",()=>{
 // rel_new is inside releasedBackups and keeps its backup copy, but not its binary: no rollback reads it.
 const jobs=[{id:"rel_new",state:"released",settledAt:"2026-09-30T12:00:00Z"},{id:"rel_old",state:"released",settledAt:"2026-09-29T12:00:00Z"},{id:"rel_refused",state:"refused"},{id:"rel_back",state:"rolled_back"},{id:"rel_super",state:"superseded"},{id:"rel_live",state:"claimed"},{id:"rel_future",state:"some_future_state"},{id:"rel_link",state:"refused"},{id:"rel_dir",state:"released",settledAt:"2026-09-28T12:00:00Z"}];
 const bin=id=>id+"-migration",j=journalFixture({[copyOf("rel_new")]:30,[copyOf("rel_old")]:30,...Object.fromEntries(["rel_new","rel_old","rel_refused","rel_back","rel_super","rel_live","rel_future"].map((id,i)=>[bin(id),41+i])),"rel_unlisted-migration":5,"target.bin":9});
 // A link or directory of that name is neither followed nor removed.
 const elsewhere=mkdtempSync(join(tmpdir(),"release-retention-elsewhere-"));writeFileSync(join(elsewhere,"binary"),"elsewhere");
 symlinkSync(join(elsewhere,"binary"),join(j.dir,bin("rel_link")));mkdirSync(join(j.dir,bin("rel_dir")));writeFileSync(join(j.dir,bin("rel_dir"),"inside"),"inside");
 try{
  const removed=sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:0}),expected=[
   {jobId:"rel_new",file:bin("rel_new"),kind:"migration",bytes:41,reason:"terminal"},{jobId:"rel_old",file:bin("rel_old"),kind:"migration",bytes:42,reason:"terminal"},
   {jobId:"rel_refused",file:bin("rel_refused"),kind:"migration",bytes:43,reason:"terminal"},{jobId:"rel_back",file:bin("rel_back"),kind:"migration",bytes:44,reason:"terminal"},
   {jobId:"rel_super",file:bin("rel_super"),kind:"migration",bytes:45,reason:"terminal"},{jobId:"rel_old",file:copyOf("rel_old"),kind:"backup",bytes:30,reason:"count"}].map(r=>({version:1,at:"2026-10-01T09:30:00.000Z",...r}));
  assert.deepEqual(removed,expected);assert.deepEqual(j.lines(),expected);
  assert.deepEqual(j.names(),[copyOf("rel_new"),bin("rel_dir"),bin("rel_future"),bin("rel_link"),bin("rel_live"),"rel_unlisted-migration","target.bin"].sort());
  assert.ok(lstatSync(join(j.dir,bin("rel_link"))).isSymbolicLink());assert.equal(readFileSync(join(elsewhere,"binary"),"utf8"),"elsewhere");
  assert.equal(readFileSync(join(j.dir,bin("rel_dir"),"inside"),"utf8"),"inside");
  assert.deepEqual(sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:0}),[],"a second sweep removes nothing");
  // The record comes first: when it cannot be written the binary stays.
  const k=journalFixture({[bin("rel_refused")]:43});mkdirSync(join(k.dir,"retention.jsonl"));
  assert.throws(()=>sweep(k,jobs,{releasedBackups:1,backupBudgetBytes:0}));assert.ok(existsSync(join(k.dir,bin("rel_refused"))));
 }finally{rmSync(elsewhere,{recursive:true,force:true});}
});
test("retention removes a terminal job's backup copies of every generation and nothing else that carries one",()=>{
 const jobs=[{id:"rel_old",state:"released",settledAt:"2026-09-30T10:00:00Z"},{id:"rel_new",state:"released",settledAt:"2026-09-30T11:00:00Z"},{id:"rel_ref",state:"refused"},{id:"rel_live",state:"claimed"}];
 const others=["rel_old.json","rel_old-g3-inputs.json","rel_old-g8-inputs.json","rel_old-g8-truenas-plan.json","rel_old-g8-truenas-preflight.json","rel_old-g8-truenas-backup.sqlite.tmp","rel_old-g8-truenas-backup.sqlite.rehearsal-rel_old.json","rel_old-g0-truenas-backup.sqlite","rel_old-gx-truenas-backup.sqlite","rel_old-g8-other-backup.sqlite","rel_older-g8-truenas-backup.sqlite","xrel_old-g8-truenas-backup.sqlite"];
 const live=["rel_live-g3-truenas-backup.sqlite","rel_live-g8-truenas-backup.sqlite","rel_live-hub-backup.sqlite"];
 const j=journalFixture({"rel_old-g3-truenas-backup.sqlite":10,"rel_old-g8-truenas-backup.sqlite":30,"rel_old-g8-hub-backup.sqlite":4,[copyOf("rel_old")]:5,"rel_old-g8-truenas-backup.sqlite.rehearsal-rel_old":7,"rel_new-g4-truenas-backup.sqlite":30,"rel_ref-g2-bridge-backup.sqlite":11,...Object.fromEntries([...others,...live].map(n=>[n,3]))});
 const removed=sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:0}),expected=[
  {jobId:"rel_old",file:"rel_old-g8-truenas-backup.sqlite.rehearsal-rel_old",kind:"rehearsal",bytes:7,reason:"terminal"},{jobId:"rel_ref",file:"rel_ref-g2-bridge-backup.sqlite",kind:"backup",bytes:11,reason:"not-released"},
  {jobId:"rel_old",file:"rel_old-g3-truenas-backup.sqlite",kind:"backup",bytes:10,reason:"count"},{jobId:"rel_old",file:"rel_old-g8-truenas-backup.sqlite",kind:"backup",bytes:30,reason:"count"},{jobId:"rel_old",file:copyOf("rel_old"),kind:"backup",bytes:5,reason:"count"},{jobId:"rel_old",file:"rel_old-g8-hub-backup.sqlite",kind:"backup",bytes:4,reason:"count"}].map(r=>({version:1,at:"2026-10-01T09:30:00.000Z",...r}));
 assert.deepEqual(removed,expected);assert.deepEqual(j.lines(),expected);
 assert.deepEqual(j.names(),[...others,...live,"rel_new-g4-truenas-backup.sqlite"].sort(),"a claimed job keeps every generation; manifests, plans and receipts stay");
 // Over budget, the newest released job's copy of each generation goes too.
 writeFileSync(join(j.dir,"rel_new-g9-truenas-backup.sqlite"),"x".repeat(30));
 assert.deepEqual(sweep(j,jobs,{releasedBackups:1,backupBudgetBytes:40}).map(r=>[r.file,r.reason]),[["rel_new-g4-truenas-backup.sqlite","budget"],["rel_new-g9-truenas-backup.sqlite","budget"]]);
 assert.deepEqual(j.names(),[...others,...live].sort());
});
test("p3 a backup or rehearsal copy that cannot be unlinked is recorded once over two sweeps and goes later without a second line",()=>{
 const jobs=[{id:"rel_ref",state:"refused"}],at="2026-10-01T09:30:00.000Z";
 // A backup copy in a journal directory that refuses the unlink; the log already exists, so its append still works.
 const b=journalFixture({"rel_ref-bridge-backup.sqlite":11}),backup={version:1,at,jobId:"rel_ref",file:"rel_ref-bridge-backup.sqlite",kind:"backup",bytes:11,reason:"not-released"};
 writeFileSync(join(b.dir,"retention.jsonl"),"",{mode:0o600});chmodSync(b.dir,0o500);
 try{for(const n of [1,2]){assert.throws(()=>sweep(b,jobs,{}),/EACCES|EPERM/,"sweep "+n);assert.deepEqual(b.lines(),[backup],"sweep "+n);}}finally{chmodSync(b.dir,0o700);}
 assert.deepEqual(b.names(),["rel_ref-bridge-backup.sqlite"]);
 assert.deepEqual(sweep(b,jobs,{}),[],"the earlier line already records it");assert.deepEqual(b.names(),[]);assert.deepEqual(b.lines(),[backup]);
 // A rehearsal file in a state directory that refuses the unlink.
 const state=copyOf("rel_ref")+".rehearsal-rel_ref.state",r=journalFixture({}),rehearsal={version:1,at,jobId:"rel_ref",file:state+"/copy.sqlite-wal",kind:"rehearsal",bytes:5,reason:"terminal"};
 mkdirSync(join(r.dir,state));writeFileSync(join(r.dir,state,"copy.sqlite-wal"),"x".repeat(5));chmodSync(join(r.dir,state),0o500);
 try{for(const n of [1,2]){assert.throws(()=>sweep(r,jobs,{}),/EACCES|EPERM/,"sweep "+n);assert.deepEqual(r.lines(),[rehearsal],"sweep "+n);}}finally{chmodSync(join(r.dir,state),0o700);}
 assert.deepEqual(sweep(r,jobs,{}),[]);assert.deepEqual(r.names(),[]);assert.deepEqual(r.lines(),[rehearsal]);
});
test("an old TailOS dist folder that cannot be removed is recorded once over two sweeps and goes later without a second line",()=>{
 // The folder refuses the removal of its own entries, so it and its index stay.
 const j=distFixture(2,3,4,5),record=distRecord("rel_2",2,"count");chmodSync(join(j.dir,DIST(2)),0o500);
 try{for(const n of [1,2]){assert.throws(()=>sweepAt(j,fiveReleased(),{}),/EACCES|EPERM|ENOTEMPTY/,"sweep "+n);assert.deepEqual(j.lines(),[record],"sweep "+n);}}finally{chmodSync(join(j.dir,DIST(2)),0o700);}
 assert.ok(existsSync(join(j.dir,DIST(2),"index.html")));
 assert.deepEqual(sweepAt(j,fiveReleased(),{}),[],"the earlier line already records it");assert.deepEqual(j.names(),[DIST(3),DIST(4),DIST(5)]);assert.deepEqual(j.lines(),[record]);
});
test("a8 retention defaults apply, an invalid value stops the daemon before any command, and a failed sweep does not hold the poll",async()=>{
 assert.deepEqual(retentionPolicy({}),{releasedBackups:3,backupBudgetBytes:4294967296,releasedDists:3,attemptDays:3});assert.deepEqual(retentionPolicy({retention:{releasedBackups:0,attemptDays:0}}),{releasedBackups:0,backupBudgetBytes:4294967296,releasedDists:3,attemptDays:0});
 const cwd=mkdtempSync(join(tmpdir(),"release-retention-daemon-")),log=join(cwd,"calls"),fakeTT=join(cwd,"tt");
 const jobs=[...["rel_1","rel_2","rel_3","rel_4"].map((id,i)=>({id,state:"released",settledAt:`2026-09-30T1${i}:00:00Z`})),{id:"next",state:"verified",generation:1}];
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify(jobs)},a)));else process.exit(2);`);chmodSync(fakeTT,0o755);
 const base={version:1,enabled:true,cwd,journalDirectory:cwd,tt:fakeTT};
 for(const retention of [{releasedBackups:-1},{releasedBackups:1.5},{releasedBackups:"3"},{backupBudgetBytes:-1},{backupBudgetBytes:0.5},{backupBudgetBytes:null},{releasedDists:-1},{releasedDists:1.5},{releasedDists:"3"},{releasedDists:null},{attemptDays:-1},{attemptDays:0.5},{attemptDays:"3"},{attemptDays:null},"3",[3]])await assert.rejects(serveDeployment({...base,retention},{once:true}),/Invalid journal retention/);
 assert.ok(!existsSync(log),"no command ran");
 // The default policy keeps three released copies: the poll removes the oldest and still tries the waiting job.
 for(const x of jobs.slice(0,4))writeFileSync(join(cwd,copyOf(x.id)),"copy");
 await serveDeployment(base,{once:true});assert.ok(!existsSync(join(cwd,copyOf("rel_1"))));assert.deepEqual(["rel_2","rel_3","rel_4"].filter(id=>existsSync(join(cwd,copyOf(id)))),["rel_2","rel_3","rel_4"]);
 assert.match(readFileSync(log,"utf8"),/deployment claim --job next /);
 // A removal record that cannot be written fails the sweep: the copy stays and the poll goes on to the claim.
 rmSync(log);rmSync(join(cwd,"retention.jsonl"));mkdirSync(join(cwd,"retention.jsonl"));
 await serveDeployment({...base,retention:{releasedBackups:0}},{once:true});assert.ok(existsSync(join(cwd,copyOf("rel_4"))));assert.match(readFileSync(log,"utf8"),/deployment claim --job next /);
});
test("t1 dist retention keeps the newest released TailOS commits and removes older copies and a copy left before them",()=>{
 // rel_old_rb rolled back before the oldest kept release (rel_3); rel_new_rb after it.
 const jobs=[...fiveReleased(),tailosJob("rel_0",0,"2026-09-20T10:00:00Z"),{id:"rel_old_rb",state:"rolled_back",settledAt:"2026-09-21T12:00:00Z",commit:C("e"),integratedCommit:C("a")},{id:"rel_new_rb",state:"rolled_back",settledAt:"2026-09-24T12:00:00Z",commit:C("b"),receipt:{commit:C("b"),outcome:"rolled_back",targets:[{target:"tailos",outcome:"rolled_back"}]}}];
 const build=()=>{
  const j=distFixture(1,2,3,4,5,"a","b","c"),elsewhere=mkdtempSync(join(tmpdir(),"release-dist-elsewhere-"));
  mkdirSync(join(j.dir,"tailos-dist-x.tmp"));writeFileSync(join(j.dir,DIST(1)+".tmp"),"partial");writeFileSync(join(elsewhere,"index.html"),"elsewhere");symlinkSync(elsewhere,join(j.dir,DIST(0)));
  return {j,elsewhere,untouched:[DIST("c"),"tailos-dist-x.tmp",DIST(1)+".tmp",DIST(0)]};
 };
 const d=build(),removed=sweepAt(d.j,jobs,{}),expected=[distRecord("rel_1",1,"count"),distRecord("rel_2",2,"count"),distRecord("rel_old_rb","a","not-released")];
 assert.deepEqual(removed,expected);assert.deepEqual(d.j.lines(),expected);
 assert.deepEqual(d.j.names(),[DIST(3),DIST(4),DIST(5),DIST("b"),...d.untouched].sort(),"the newest three stay, and so does a copy no listed job accounts for");
 assert.equal(readFileSync(join(d.elsewhere,"index.html"),"utf8"),"elsewhere","a link named like a copy is neither followed nor removed");
 assert.equal(readFileSync(join(d.j.dir,DIST(5),"assets","app.js"),"utf8").length,5);
 assert.deepEqual(sweepAt(d.j,jobs,{}),[],"a second sweep removes nothing");
 // At zero only the newest released commit and a named copy stay; a superseded job that cites a TailOS hand release is the newest.
 const z=build(),hand={id:"rel_hand",state:"superseded",settledAt:"2026-09-26T10:00:00Z",supersession:{releasedCommit:C(6),targets:["tailos"]}};addDist(z.j,6);
 assert.deepEqual(sweepAt(z.j,[...jobs,hand],{releasedDists:0},{keepDists:[C(2),undefined,"not a commit"]}).map(r=>[r.jobId,r.file,r.reason]),[["rel_1",DIST(1),"count"],["rel_3",DIST(3),"count"],["rel_4",DIST(4),"count"],["rel_5",DIST(5),"count"],["rel_old_rb",DIST("a"),"not-released"],["rel_new_rb",DIST("b"),"not-released"]]);
 assert.deepEqual(z.j.names(),[DIST(2),DIST(6),...z.untouched].sort());
 // With no released TailOS job in the list nothing is known to be old.
 const n=distFixture(1,"a");assert.deepEqual(sweepAt(n,jobs.filter(x=>x.state!=="released"),{releasedDists:0}),[]);assert.deepEqual(n.names(),[DIST(1),DIST("a")]);
});
test("t2 a dist named by a non-terminal job's inputs or rollback program stays until that job is terminal",()=>{
 const pending={id:"rel_next",state:"claimed",commit:C(8),integratedCommit:C(9)},jobs=state=>[...fiveReleased(),{...pending,state,...(state==="claimed"?{}:{settledAt:"2026-09-30T10:00:00Z"})}];
 const manifest=(j,n)=>JSON.stringify({version:1,jobId:"rel_next",targets:{tailos:{rollbackSafe:true,rollbackProgram:["npx","wrangler","pages","deploy",join(j.dir,DIST(n)),"--project-name","tailos"]}}});
 const j=distFixture(1,2,3,4,5,8,9);writeFileSync(join(j.dir,"rel_next-g2-inputs.json"),manifest(j,1));writeFileSync(join(j.dir,"rel_1-g1-inputs.json"),manifest(j,2));writeFileSync(join(j.dir,"xrel_next-g2-inputs.json"),manifest(j,2));
 // rel_1's copy is named by the claimed job; rel_2's only by a terminal job's manifest and another job's.
 assert.deepEqual(sweepAt(j,jobs("claimed"),{}),[distRecord("rel_2",2,"count")]);
 for(const n of [1,8,9])assert.ok(existsSync(join(j.dir,DIST(n),"index.html")),"kept "+n);
 for(const state of ["verified","merged","blocked","some_future_state"])assert.deepEqual(sweepAt(j,jobs(state),{}),[],state);
 assert.deepEqual(sweepAt(j,jobs("refused"),{}),[distRecord("rel_1",1,"count")],"once the job is terminal its named copy goes");
 // A manifest from before names carried a generation protects too.
 const legacy=distFixture(1,3,4,5);writeFileSync(join(legacy.dir,"rel_next-inputs.json"),manifest(legacy,1));assert.deepEqual(sweepAt(legacy,jobs("claimed"),{}),[]);
 // A non-terminal job's manifest that cannot be read as a regular file keeps every copy.
 const unreadable=distFixture(1,2,3,4,5);mkdirSync(join(unreadable.dir,"rel_next-g3-inputs.json"));
 const linked=distFixture(1,2,3,4,5);writeFileSync(join(linked.dir,"elsewhere.json"),"{}");symlinkSync(join(linked.dir,"elsewhere.json"),join(linked.dir,"rel_next-g3-inputs.json"));
 for(const k of [unreadable,linked]){assert.deepEqual(sweepAt(k,jobs("claimed"),{releasedDists:0}),[]);assert.equal(k.names().filter(n=>n.startsWith("tailos-dist-")).length,5);assert.deepEqual(k.lines(),[]);}
 assert.equal(sweepAt(unreadable,jobs("released"),{releasedDists:0}).length,4,"the same journal once that job is terminal");
});
test("t3 a dist that holds a link stays whole, with the link's target intact and no record",()=>{
 const j=distFixture(1,2,3,4,5),outside=join(mkdtempSync(join(tmpdir(),"release-dist-outside-")),"kept");writeFileSync(outside,"outside");
 symlinkSync(outside,join(j.dir,DIST(1),"assets","link"));symlinkSync(dirname(outside),join(j.dir,DIST(1),"linked-directory"));
 assert.deepEqual(sweepAt(j,fiveReleased(),{}),[distRecord("rel_2",2,"count")]);assert.deepEqual(j.lines(),[distRecord("rel_2",2,"count")]);
 assert.deepEqual(readdirSync(join(j.dir,DIST(1))).sort(),["assets","index.html","linked-directory"]);assert.deepEqual(readdirSync(join(j.dir,DIST(1),"assets")).sort(),["app.js","link"]);
 assert.equal(readFileSync(outside,"utf8"),"outside");
});
test("t4 attempt retention removes old files that are not evidence, only from a terminal job's ended attempts",()=>{
 const jobs=[{id:"rel_live",state:"claimed"},{id:"rel_future",state:"some_future_state"},{id:"rel_started",state:"released"},{id:"rel_broken",state:"refused"},{id:"rel_ended",state:"released"},{id:"rel_aside",state:"rolled_back"},{id:"rel_none",state:"superseded"}];
 const j=journalFixture({}),run=state=>({"run.json":[JSON.stringify({version:1,state}),9]}),all={...EVIDENCE,...BULK,...YOUNG},dirs={};
 for(const [id,record] of [["rel_live",run("ended")],["rel_future",run("ended")],["rel_started",run("started")],["rel_broken",{"run.json":["not json",9]}],["rel_ended",run("ended")],["rel_aside",{"run.json.set-aside":[JSON.stringify({version:1,state:"started"}),9]}],["rel_none",{}]])dirs[id]={dir:addAttempt(j,id,{...all,...record}),names:Object.keys({...all,...record}).sort()};
 // A second attempt of the same job is judged by its own record.
 const second=addAttempt(j,"rel_ended",{...all,...run("starting")},C(7)+"-r1");
 // Files directly in the parent (the layout before attempts) and a directory that is not an attempt are left.
 writeFileSync(join(j.dir,"rel_ended-integrated-verification","tailterm-hub-test"),"flat");mkdirSync(join(j.dir,"rel_ended-integrated-verification","notes"));writeFileSync(join(j.dir,"rel_ended-integrated-verification","notes","run.out"),"old");
 const removed=sweepAt(j,jobs,{}),expected=["rel_ended","rel_aside","rel_none"].flatMap(bulkRecords);
 assert.deepEqual(removed,expected);assert.deepEqual(j.lines(),expected);
 for(const id of ["rel_live","rel_future","rel_started","rel_broken"])assert.deepEqual(readdirSync(dirs[id].dir).sort(),dirs[id].names,id+" loses nothing");
 assert.equal(readdirSync(second).length,Object.keys(all).length+1,"an attempt whose run has not ended loses nothing");
 for(const id of ["rel_ended","rel_aside","rel_none"]){
  assert.deepEqual(readdirSync(dirs[id].dir).sort(),dirs[id].names.filter(n=>!(n in BULK)),id);
  for(const [name,[content]] of Object.entries({...EVIDENCE,...YOUNG}))assert.equal(readFileSync(join(dirs[id].dir,name),"utf8"),content,id+" "+name);
 }
 assert.equal(readFileSync(join(dirs.rel_ended.dir,"run.json"),"utf8"),JSON.stringify({version:1,state:"ended"}));assert.ok(existsSync(join(dirs.rel_aside.dir,"run.json.set-aside")));
 assert.deepEqual(readdirSync(join(j.dir,"rel_ended-integrated-verification")).sort(),[C(7)+"-r0",C(7)+"-r1","notes","tailterm-hub-test"]);
 assert.deepEqual(sweepAt(j,jobs,{}),[],"a second sweep removes nothing; the two-day-old file is younger than attemptDays");
 assert.deepEqual(sweepAt(j,jobs,{attemptDays:2}),["rel_ended","rel_aside","rel_none"].map(id=>attemptRecord(id,"tailterm-tt-test",20)));
 // At zero a terminal job's ended attempt keeps only its evidence, however new the rest is.
 const fresh=journalFixture({}),dir=addAttempt(fresh,"rel_ended",{...EVIDENCE,...run("ended"),"tailterm-hub-test":["h",0]});
 assert.deepEqual(sweepAt(fresh,jobs,{}),[]);assert.deepEqual(sweepAt(fresh,jobs,{attemptDays:0}),[attemptRecord("rel_ended","tailterm-hub-test",1)]);assert.deepEqual(readdirSync(dir).sort(),[...Object.keys(EVIDENCE),"run.json"].sort());
});
test("t5 attempt retention follows and removes no link, and enters no linked or misplaced directory",()=>{
 const j=journalFixture({}),elsewhere=mkdtempSync(join(tmpdir(),"release-attempt-elsewhere-")),old=new Date(SWEEP_AT-9*86400000),ended={"run.json":[JSON.stringify({version:1,state:"ended"}),9]};
 const outside=join(elsewhere,"binary");writeFileSync(outside,"outside");utimesSync(outside,old,old);
 const jobs=["rel_in","rel_attempt","rel_parent","rel_file","rel_record","rel_absent"].map(id=>({id,state:"released"}));
 // A link and a subdirectory inside an ended attempt.
 const inside=addAttempt(j,"rel_in",{...ended,"run.out":["out",9]});symlinkSync(outside,join(inside,"tailterm-hub-test"));mkdirSync(join(inside,"sub"));writeFileSync(join(inside,"sub","tailterm-tt-test"),"nested");utimesSync(join(inside,"sub","tailterm-tt-test"),old,old);
 // An attempt directory that is a link, and a parent that is a link, each to a directory with an old binary.
 const target=join(elsewhere,"attempt");mkdirSync(target);writeFileSync(join(target,"tailterm-hub-test"),"linked attempt");utimesSync(join(target,"tailterm-hub-test"),old,old);
 mkdirSync(join(j.dir,"rel_attempt-integrated-verification"));symlinkSync(target,join(j.dir,"rel_attempt-integrated-verification",C(7)+"-r0"));
 const parent=join(elsewhere,"parent");mkdirSync(join(parent,C(7)+"-r0"),{recursive:true});writeFileSync(join(parent,C(7)+"-r0","tailterm-hub-test"),"linked parent");utimesSync(join(parent,C(7)+"-r0","tailterm-hub-test"),old,old);
 symlinkSync(parent,join(j.dir,"rel_parent-integrated-verification"));
 // A parent that is a regular file, and a run record that is a link to an ended one.
 writeFileSync(join(j.dir,"rel_file-integrated-verification"),"not a directory");
 const record=addAttempt(j,"rel_record",{"tailterm-hub-test":["binary",9]});writeFileSync(join(elsewhere,"run.json"),JSON.stringify({version:1,state:"ended"}));symlinkSync(join(elsewhere,"run.json"),join(record,"run.json"));
 assert.deepEqual(sweepAt(j,jobs,{attemptDays:0}),[attemptRecord("rel_in","run.out",3)]);
 assert.deepEqual(readdirSync(inside).sort(),["run.json","sub","tailterm-hub-test"]);assert.equal(readFileSync(outside,"utf8"),"outside");assert.equal(readFileSync(join(inside,"sub","tailterm-tt-test"),"utf8"),"nested");
 assert.equal(readFileSync(join(target,"tailterm-hub-test"),"utf8"),"linked attempt");assert.equal(readFileSync(join(parent,C(7)+"-r0","tailterm-hub-test"),"utf8"),"linked parent");
 assert.equal(readFileSync(join(j.dir,"rel_file-integrated-verification"),"utf8"),"not a directory");assert.deepEqual(readdirSync(record).sort(),["run.json","tailterm-hub-test"]);assert.ok(existsSync(join(elsewhere,"run.json")));
});
test("t6 every dist and attempt removal is one retention.jsonl line written before it, after the backup records",()=>{
 const jobs=fiveReleased(),j=distFixture(1,2,3,4,5);writeFileSync(join(j.dir,copyOf("rel_1")),"x".repeat(30));
 addAttempt(j,"rel_1",{...EVIDENCE,"tailterm-hub-test":["h".repeat(40),9],"run.out":["out",9]});
 const expected=[{version:1,at:"2026-10-01T09:30:00.000Z",jobId:"rel_1",file:copyOf("rel_1"),kind:"backup",bytes:30,reason:"count"},distRecord("rel_1",1,"count"),distRecord("rel_2",2,"count"),attemptRecord("rel_1","run.out",3),attemptRecord("rel_1","tailterm-hub-test",40)];
 // Each record is already on disk when its removal happens: a sweep stopped at the second dist leaves two lines and that dist.
 const real=join(j.dir,"retention.jsonl");let calls=0;
 assert.throws(()=>pruneJournal({journalDirectory:j.dir,retention:{}},jobs,{now:()=>{if(++calls===3){assert.deepEqual(j.lines(),expected.slice(0,2));assert.ok(!existsSync(join(j.dir,DIST(1))));assert.ok(existsSync(join(j.dir,DIST(2))));throw new Error("stopped");}return SWEEP_AT;}}),/stopped/);
 assert.deepEqual(j.lines(),expected.slice(0,2));assert.ok(existsSync(join(j.dir,DIST(2),"index.html")),"a failed sweep removes nothing further");
 assert.deepEqual(sweepAt(j,jobs,{}),expected.slice(2));assert.deepEqual(j.lines(),expected);
 for(const line of j.lines())assert.deepEqual(Object.keys(line),["version","at","jobId","file","kind","bytes","reason"]);
 assert.equal(statSync(real).mode&0o777,0o600);
 assert.deepEqual(sweepAt(j,jobs,{}),[],"a second sweep removes nothing");assert.deepEqual(j.lines(),expected);
});
test("t7 the daemon sweeps dists and attempts at a poll, and a failed sweep keeps them, prints the notice and still claims",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-dist-daemon-")),home=join(cwd,"journal"),log=join(cwd,"calls"),fakeTT=join(cwd,"tt");mkdirSync(home);
 const jobs=[...fiveReleased(),{id:"next",state:"verified",generation:1}],old=new Date(Date.now()-10*86400000);
 writeFileSync(fakeTT,"#!"+process.execPath+"\n"+releaseReplySource+`const fs=require('fs');const a=process.argv.slice(2);fs.appendFileSync(${JSON.stringify(log)},a.join(' ')+'\\n');if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify(jobs)},a)));else process.exit(2);`);chmodSync(fakeTT,0o755);
 const base={version:1,enabled:true,cwd,journalDirectory:home,tt:fakeTT},j={dir:home},binary=join(home,"rel_1-integrated-verification",C(7)+"-r0","tailterm-hub-test");
 const build=()=>{for(const n of [1,2,3,4,5])addDist(j,n);mkdirSync(dirname(binary),{recursive:true});writeFileSync(binary,"binary");utimesSync(binary,old,old);writeFileSync(join(dirname(binary),"receipt.json"),"receipt");utimesSync(join(dirname(binary),"receipt.json"),old,old);};
 const said=[],write=process.stderr.write;process.stderr.write=text=>{said.push(String(text));return true;};
 try{
  // The record cannot be written: every copy and file stays, the one-line notice is printed and the poll goes on to the claim.
  build();mkdirSync(join(home,"retention.jsonl"));
  await serveDeployment(base,{once:true});
  for(const n of [1,2,3,4,5])assert.ok(existsSync(join(home,DIST(n),"assets","app.js")),"kept "+n);assert.ok(existsSync(binary));
  assert.ok(said.includes("Journal retention sweep failed; remaining backup copies kept.\n"));assert.match(readFileSync(log,"utf8"),/deployment claim --job next /);
  // With the record writable, the default policy applies at the next poll.
  rmSync(log);rmSync(join(home,"retention.jsonl"),{recursive:true});said.length=0;
  await serveDeployment(base,{once:true});
  assert.deepEqual(readdirSync(home).filter(n=>n.startsWith("tailos-dist-")).sort(),[DIST(3),DIST(4),DIST(5)]);assert.deepEqual(readdirSync(dirname(binary)),["receipt.json"]);
  assert.ok(!said.some(text=>text.includes("retention sweep failed")));assert.match(readFileSync(log,"utf8"),/deployment claim --job next /);
  assert.deepEqual(readFileSync(join(home,"retention.jsonl"),"utf8").split("\n").filter(Boolean).map(l=>{const r=JSON.parse(l);return [r.jobId,r.file,r.kind,r.reason];}),[["rel_1",DIST(1),"dist","count"],["rel_2",DIST(2),"dist","count"],["rel_1","rel_1-integrated-verification/"+C(7)+"-r0/tailterm-hub-test","attempt","age"]]);
 }finally{process.stderr.write=write;}
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
 const r=await runRelease(config(f,j),a);assert.equal(r.outcome,"released");assert.equal(remoteHead(f),diverged);
 const {elapsedMs,stderrSHA256,...push}=r.push;assert.deepEqual(push,{remote:"origin",commit:j.commit,outcome:"failed",exitCode:1,reason:"push-rejected",remoteCheck:"absent"});
 assert.ok(Number.isSafeInteger(elapsedMs)&&elapsedMs>=0);assert.match(stderrSHA256,/^[a-f0-9]{64}$/);
 assert.deepEqual(escalation,{jobId:"rel_fixture",outcome:"released",push:"failed",pushCause:{exitCode:1,elapsedMs,reason:"push-rejected",remoteCheck:"absent",stderrSHA256}});assert.deepEqual(a.calls,["merged","deploy:tailos","escalate","finish"]);
});
// wi_8afe404c4867dafc p5/p5c: a fake git that runs the real git for every
// call except the push (and, when given, ls-remote), against the fixture's
// temp bare remote only. $D is the fake's own temp directory.
const REAL_GIT=execFileSync("sh",["-c","command -v git"],{encoding:"utf8"}).trim();
function fakeGit(push,lsRemote){
 const dir=mkdtempSync(join(tmpdir(),"release-fake-git-")),path=join(dir,"git");
 writeFileSync(join(dir,"stderr.txt"),"fatal: unable to access 'https://x-access-token:ghs_0123456789abcdefABCDEF0123456789@github.com/o/r.git/': The requested URL returned error: 403\npassword=hunter2hunter2\n");
 writeFileSync(path,`#!/bin/sh\nREAL='${REAL_GIT}'\nERR='${join(dir,"stderr.txt")}'\nD='${dir}'\ncase "$1" in\n push) ${push} ;;\n ls-remote) ${lsRemote||":"} ;;\nesac\nexec "$REAL" "$@"\n`);chmodSync(path,0o755);return path;
}
const PUSH_MODES={slow:"sleep 60; exit 0",fail:'cat "$ERR" >&2; exit 128',landed:'"$REAL" "$@" || exit $?; echo "error: RPC failed; curl 92 HTTP/2 stream 0 was not closed cleanly" >&2; exit 1'};
// A release through the fake git; finish writes the receipt file as the host
// adapter does, so the file and the journal can be read separately.
async function pushCase(push,lsRemote,{timeoutMs=60000,graceMs,stopMs,setup}={}){
 const gitPath=fakeGit(push,lsRemote),f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c={...config(f,j),push:{git:gitPath,timeoutMs,graceMs,stopMs,checkTimeoutMs:10000}},escalations=[];
 const host=new HostAdapter({cwd:f.cwd,journalDirectory:dirname(c.journalPath)},j);host.native=()=>host.job;
 a.escalate=async d=>{escalations.push(d);a.calls.push("escalate");};a.finish=async(r,g)=>{await host.finish(r,g);a.calls.push("finish");};
 if(setup)setup(f,c);
 const started=Date.now(),r=await runRelease(c,a),took=Date.now()-started;
 return {f,j,a,c,r,took,escalations,dir:dirname(gitPath),file:existsSync(join(dirname(c.journalPath),"rel_fixture-receipt.json"))?readFileSync(join(dirname(c.journalPath),"rel_fixture-receipt.json"),"utf8"):null,journal:JSON.parse(readFileSync(c.journalPath,"utf8"))};
}
const STDERR_TEXT=["unable to access","RPC failed","[redacted]","hunter2","ghs_","fatal","error:"];
const noStderr=(text,what)=>{for(const s of STDERR_TEXT)assert.ok(!text.includes(s),`${what} holds ${s}`);};
// The slow push would run 60 s, so a run well inside that did not wait for
// it, however slowly a loaded host runs the rest of the release.
test("push p5 a slow push past its limit is recorded as timed out with its elapsed time, without hanging the runner",async()=>{
 const t=await pushCase(PUSH_MODES.slow,undefined,{timeoutMs:500});
 assert.equal(t.r.outcome,"released");assert.ok(t.took<45000,String(t.took));assert.equal(remoteHead(t.f),t.f.base);
 const {elapsedMs,...push}=t.r.push;assert.deepEqual(push,{remote:"origin",commit:t.j.commit,outcome:"failed",timedOut:true,signal:"sigterm",reason:"push-timed-out",remoteCheck:"absent"});
 assert.ok(elapsedMs>=500&&elapsedMs<45000,String(elapsedMs));assert.equal(t.escalations.length,1);assert.deepEqual(t.a.calls,["merged","deploy:tailos","escalate","finish"]);
 const attempt=t.journal.pushAttempts.at(-1);assert.equal(attempt.timedOut,true);assert.equal(attempt.elapsedMs,elapsedMs);assert.equal(attempt.remoteCheck,"absent");
 assert.deepEqual(JSON.parse(t.file).push,t.r.push);
 const cwd=mkdtempSync(join(tmpdir(),"escalate-push-")),calls=[],adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate(t.escalations[0]);const text=calls[0][calls[0].indexOf("--text")+1];
 assert.equal(calls[0][calls[0].indexOf("--request-id")+1],"rel_fixture-push-failure");assert.match(text,/Cause: timed out after \d+ s \(push-timed-out\)\. Remote check: absent\./);
});
test("push p5 a failing push keeps its exit code and its redacted stderr in the journal, never in the receipt",async()=>{
 const t=await pushCase(PUSH_MODES.fail);
 assert.equal(t.r.outcome,"released");assert.equal(remoteHead(t.f),t.f.base);
 const {elapsedMs,stderrSHA256,...push}=t.r.push;assert.deepEqual(push,{remote:"origin",commit:t.j.commit,outcome:"failed",exitCode:128,reason:"push-auth-failed",remoteCheck:"absent"});
 const attempt=t.journal.pushAttempts.at(-1),stderr=attempt.stderr;
 assert.equal(stderrSHA256,createHash("sha256").update(stderr).digest("hex"));assert.equal(attempt.stderrSHA256,stderrSHA256);assert.equal(attempt.exitCode,128);
 assert.match(stderr,/unable to access/);assert.match(stderr,/\[redacted\]/);assert.match(stderr,/https:\/\/\[redacted\]@github\.com/);
 const raw=readFileSync(t.c.journalPath,"utf8");for(const secret of ["ghs_","hunter2","x-access-token"])assert.ok(!raw.includes(secret),secret);
 noStderr(JSON.stringify(t.r),"receipt");noStderr(t.file,"receipt file");noStderr(JSON.stringify(t.escalations),"escalation");
 assert.equal(t.escalations.length,1);
 const cwd=mkdtempSync(join(tmpdir(),"escalate-push-")),calls=[],adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate(t.escalations[0]);const text=calls[0][calls[0].indexOf("--text")+1];
 assert.match(text,/Cause: exit 128 after \d+ s \(push-auth-failed\)\. Remote check: absent\. The sanitized stderr is in the private journal \(sha256 [a-f0-9]{12}\)\./);noStderr(text,"notice");
});
test("push p5 a push that landed but reported failure is found by the one re-check and not escalated",async()=>{
 const t=await pushCase(PUSH_MODES.landed);
 assert.equal(t.r.outcome,"released");assert.equal(remoteHead(t.f),t.j.commit);
 const {elapsedMs,stderrSHA256,...push}=t.r.push;assert.deepEqual(push,{remote:"origin",commit:t.j.commit,outcome:"failed",exitCode:1,reason:"push-network-failed",remoteCheck:"landed"});
 assert.deepEqual(t.escalations,[]);assert.deepEqual(t.a.calls,["merged","deploy:tailos","finish"]);assert.equal(t.journal.pushEscalated,undefined);
 assert.match(t.journal.pushAttempts.at(-1).stderr,/RPC failed/);noStderr(JSON.stringify(t.r),"receipt");noStderr(t.file,"receipt file");
});
test("push p5 a failed push whose remote cannot be read is unknown and escalated once",async()=>{
 const t=await pushCase(PUSH_MODES.fail,"exit 2");
 assert.equal(t.r.push.remoteCheck,"unknown");assert.equal(t.r.push.outcome,"failed");assert.equal(t.escalations.length,1);assert.equal(t.escalations[0].pushCause.remoteCheck,"unknown");
});
// wi_01b550d20e610c85: the push runs as its own process-group leader. A
// timed-out push whose child would land later (once the test writes $D/land,
// after the run) is stopped with its whole group, and the re-check (which
// fails as unknown if that child still runs) reads the remote only after the
// group is gone. Limits leave the fake shell time to start, so its child and
// traps are in place before the stop. A child that survives a regression
// gives up after about 60 s rather than polling on the host.
const LATE='( i=0; while [ ! -f "$D/land" ]; do [ $i -lt 1200 ] || exit 0; sleep 0.05; i=$((i+1)); done; "$REAL" "$@" ) & echo $! > "$D/child.pid"; sleep 30',ALIVE='kill -0 "$(cat "$D/child.pid")" 2>/dev/null && exit 3';
const lateLanding=async t=>{writeFileSync(join(t.dir,"land"),"");await new Promise(r=>setTimeout(r,500));};
// A timed-out push that lands as it is stopped: its SIGTERM trap runs the real
// push, then the shell dies of that signal. It lands after the timeout and
// before the re-check on any host; the grace leaves the push time to finish.
const LANDS_ON_STOP='trap \'"$REAL" "$@"; trap - TERM; kill -TERM $$\' TERM; touch "$D/armed"; sleep 30 & wait';
// A failed push that leaves a child ignoring SIGTERM: the child sets its trap
// before the push exits, so the stop never races the shell's start-up.
const STUBBORN='( trap "" TERM; touch "$D/ready"; sleep 30 ) & echo $! > "$D/child.pid"; while [ ! -f "$D/ready" ]; do sleep 0.05; done; exit 1';
const stubbornGone=dir=>{const pid=Number(readFileSync(join(dir,"child.pid"),"utf8"));assert.ok(pid>0);try{process.kill(pid,0);return false;}catch(error){return error.code==="ESRCH";}};
test("push t1 a timed-out push whose child would land later is stopped with its group, never lands, and is escalated as absent",async()=>{
 const t=await pushCase(LATE,ALIVE,{timeoutMs:2000});
 const {elapsedMs,...push}=t.r.push;assert.deepEqual(push,{remote:"origin",commit:t.j.commit,outcome:"failed",timedOut:true,signal:"sigterm",reason:"push-timed-out",remoteCheck:"absent"});
 assert.equal(t.escalations.length,1);assert.equal(t.escalations[0].pushCause.remoteCheck,"absent");assert.equal(t.journal.pushAttempts.at(-1).stopped,true);
 const child=Number(readFileSync(join(t.dir,"child.pid"),"utf8"));assert.throws(()=>process.kill(child,0),{code:"ESRCH"});
 await lateLanding(t);assert.equal(remoteHead(t.f),t.f.base,"no late landing once the child may land");
});
test("push t2 a timed-out push that landed before the re-check is recorded landed and not escalated",async()=>{
 const t=await pushCase(LANDS_ON_STOP,ALIVE,{timeoutMs:2000,graceMs:30000,stopMs:60000});
 assert.ok(existsSync(join(t.dir,"armed")),"the push set its trap before the stop");
 const {elapsedMs,...push}=t.r.push;assert.deepEqual(push,{remote:"origin",commit:t.j.commit,outcome:"failed",timedOut:true,signal:"sigterm",reason:"push-timed-out",remoteCheck:"landed"});
 assert.deepEqual(t.escalations,[]);assert.equal(t.journal.pushEscalated,undefined);assert.equal(remoteHead(t.f),t.j.commit);
});
test("push t2 a timed-out push whose remote cannot be read after the stop is unknown and escalated once",async()=>{
 const t=await pushCase(LATE,"exit 2",{timeoutMs:2000});
 assert.equal(t.r.push.remoteCheck,"unknown");assert.equal(t.r.push.timedOut,true);assert.equal(t.escalations.length,1);assert.equal(t.escalations[0].pushCause.remoteCheck,"unknown");
 await lateLanding(t);assert.equal(remoteHead(t.f),t.f.base);
});
test("push t2 a push group that outlives the stop bound is unknown without a read and escalated once",async()=>{
 const t=await pushCase(STUBBORN,'touch "$D/read"',{graceMs:4000,stopMs:300});
 try{
  assert.equal(t.r.push.remoteCheck,"unknown");assert.equal(t.r.push.exitCode,1);assert.equal(t.escalations.length,1);assert.equal(t.escalations[0].pushCause.remoteCheck,"unknown");
  assert.equal(t.journal.pushAttempts.at(-1).stopped,false);assert.ok(!existsSync(join(t.dir,"read")),"no re-check while the group runs");assert.equal(stubbornGone(t.dir),false);
 }finally{try{process.kill(Number(readFileSync(join(t.dir,"child.pid"),"utf8")),"SIGKILL");}catch{}}
});
test("push t1 a failed push's child that ignores SIGTERM is killed after the grace, and the stop bounds are named",async()=>{
 assert.equal(PUSH_STOP_GRACE_MS,5000);assert.equal(PUSH_STOP_WAIT_MS,15000);
 const git=fakeGit(STUBBORN),r=await pushRelease(mkdtempSync(join(tmpdir(),"push-group-")),"a".repeat(40),{git,graceMs:300,stopMs:5000});
 const {elapsedMs,...rest}=r;assert.deepEqual(rest,{outcome:"failed",exitCode:1,reason:"push-failed",stopped:true});assert.ok(elapsedMs>=300,"SIGKILL waited for the grace");
 assert.equal(stubbornGone(dirname(git)),true);
});
test("push p4 a run stopped in pushing after a failed attempt pushes once more and escalates at most once",async()=>{
 const t=await pushCase(PUSH_MODES.fail);assert.equal(t.escalations.length,1);
 const journal=JSON.parse(readFileSync(t.c.journalPath,"utf8"));journal.phase="pushing";delete journal.receipt.push;writeFileSync(t.c.journalPath,JSON.stringify(journal));
 const r=await runRelease(t.c,t.a);assert.equal(r.outcome,"released");assert.equal(r.push.outcome,"failed");
 assert.equal(t.escalations.length,1,"escalated at most once");assert.equal(t.a.calls.filter(x=>x==="deploy:tailos").length,1);
 const after=JSON.parse(readFileSync(t.c.journalPath,"utf8"));assert.equal(after.pushAttempts.length,2);assert.equal(after.phase,"complete");
 // A good push on the next resume keeps the old bare shape.
 after.phase="pushing";delete after.receipt.push;writeFileSync(t.c.journalPath,JSON.stringify(after));
 const good=await runRelease({...t.c,push:undefined},t.a);assert.deepEqual(good.push,{remote:"origin",commit:t.j.commit,outcome:"pushed"});assert.equal(remoteHead(t.f),t.j.commit);
});
// wi_56e50fb1bc6bbdaa: a run stopped mid-push leaves its push group
// journaled with process-instance proof, and the resume settles it first.
// The fake push and ls-remote log each call to $D/log; files in $D choose
// the push: fail or landed. An earlier group is a detached shell like the runner's push that
// logs the SIGTERM it gets, so the log shows the order of stop, read and push.
const RESUMABLE='echo push >> "$D/log"; [ -f "$D/fail" ] && { cat "$ERR" >&2; exit 128; }; [ -f "$D/landed" ] && { "$REAL" "$@" || exit $?; echo "error: RPC failed" >&2; exit 1; }; :';
const LOGGED_READ='echo ls-remote >> "$D/log"',TRAPPED='trap \'echo earlier-term >> "$D/log"; exit 143\' TERM; touch "$D/ready"; sleep 30 & wait';
const modes=(...names)=>(f,c)=>{for(const n of names)writeFileSync(join(dirname(c.push.git),n),"");};
const pushLog=t=>{try{return readFileSync(join(t.dir,"log"),"utf8").split("\n").filter(Boolean);}catch{return [];}};
const waitFor=async(check,ms=10000)=>{const end=Date.now()+ms;while(!check()){if(Date.now()>end)throw new Error("timed out waiting");await new Promise(r=>setTimeout(r,20));}};
async function earlierGroup(t,script,{cwd=t.dir,proof=true}={}){
 rmSync(join(t.dir,"ready"),{force:true});
 const child=spawn("sh",["-c",script],{cwd,detached:true,stdio:"ignore",env:{...process.env,D:t.dir,REAL:REAL_GIT,C:t.j.commit}});child.unref();
 await waitFor(()=>existsSync(join(t.dir,"ready")));
 return {pgid:child.pid,startedAt:proof?processStartTime(child.pid):null,boot:proof?bootIdentity():null};
}
// The journal as a run stopped in pushing leaves it, with the given group.
const resumeAt=(t,pushGroup)=>{const journal=JSON.parse(readFileSync(t.c.journalPath,"utf8"));journal.phase="pushing";delete journal.receipt.push;if(pushGroup)journal.pushGroup=pushGroup;writeFileSync(t.c.journalPath,JSON.stringify(journal));writeFileSync(join(t.dir,"log"),"");};
const readJournalFile=t=>JSON.parse(readFileSync(t.c.journalPath,"utf8"));
const killGroup=pgid=>{try{process.kill(-pgid,"SIGKILL");}catch{}};
test("push r1 the push group and its proof are journaled before the runner waits on the push, and never reach the receipt",async t0=>{
 process.env.RELEASE_TEST_JOURNAL="";t0.after(()=>{delete process.env.RELEASE_TEST_JOURNAL;});
 const seen='echo $$ > "$D/leader.pid"; i=0; while ! grep -q pushGroup "$RELEASE_TEST_JOURNAL" 2>/dev/null && [ $i -lt 100 ]; do sleep 0.05; i=$((i+1)); done; cp "$RELEASE_TEST_JOURNAL" "$D/seen.json"';
 const t=await pushCase(seen,undefined,{setup:(f,c)=>{process.env.RELEASE_TEST_JOURNAL=c.journalPath;}});
 const during=JSON.parse(readFileSync(join(t.dir,"seen.json"),"utf8")),leader=Number(readFileSync(join(t.dir,"leader.pid"),"utf8"));
 assert.equal(during.phase,"pushing");assert.equal(during.pushGroup.pgid,leader);assert.match(during.pushGroup.startedAt,ISO_START);
 assert.equal(during.pushGroup.boot,bootIdentity());assert.ok(bootIdentity());
 assert.deepEqual(t.r.push,{remote:"origin",commit:t.j.commit,outcome:"pushed"});assert.equal(t.journal.pushGroup,undefined,"a finished push leaves no group");
 for(const text of [JSON.stringify(t.r),t.file])for(const word of ["pushGroup","pgid","startedAt","boot"])assert.ok(!text.includes(word),word);
});
test("push r2 a resume stops a live earlier push group that is ours, then reads the remote, then pushes",async()=>{
 const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("fail")});assert.equal(t.escalations.length,1);rmSync(join(t.dir,"fail"));
 const group=await earlierGroup(t,TRAPPED);
 try{
  assert.equal(pushGroupState(group),"ours");resumeAt(t,group);
  const r=await runRelease(t.c,t.a);
  assert.deepEqual(pushLog(t),["earlier-term","ls-remote","push"],"stop, then read, then push");
  assert.deepEqual(r.push,{remote:"origin",commit:t.j.commit,outcome:"pushed"});assert.equal(remoteHead(t.f),t.j.commit);assert.equal(groupGone(group.pgid),true);
  const after=readJournalFile(t);assert.equal(after.pushGroup,undefined);assert.deepEqual(after.pushAttempts.at(-1).earlierGroup,{pgid:group.pgid,proof:"ours",stopped:true,remoteCheck:"absent"});
  assert.equal(t.escalations.length,1,"no second escalation");assert.equal(after.phase,"complete");
 }finally{killGroup(group.pgid);}
});
test("push r2 a resume whose stopped earlier push had landed reads it landed and does not push again",async()=>{
 const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("fail")});rmSync(join(t.dir,"fail"));
 const group=await earlierGroup(t,`"$REAL" push --quiet origin "$C:refs/heads/tasks-hub" || exit 1; ${TRAPPED}`,{cwd:t.f.cwd});
 try{
  assert.equal(remoteHead(t.f),t.j.commit);resumeAt(t,group);
  const r=await runRelease(t.c,t.a);
  assert.deepEqual(pushLog(t),["earlier-term","ls-remote"],"no new push");assert.deepEqual(r.push,{remote:"origin",commit:t.j.commit,outcome:"pushed"});
  assert.deepEqual(readJournalFile(t).pushAttempts.at(-1),{at:readJournalFile(t).pushAttempts.at(-1).at,outcome:"pushed",earlierGroup:{pgid:group.pgid,proof:"ours",stopped:true,remoteCheck:"landed"}});
  assert.equal(t.escalations.length,1);
 }finally{killGroup(group.pgid);}
});
test("push r2 an earlier group that outlives the stop bound is unknown, unread, not pushed again and escalated once",async()=>{
 const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("landed")});assert.deepEqual(t.escalations,[]);rmSync(join(t.dir,"landed"));
 const group=await earlierGroup(t,'trap "" TERM; touch "$D/ready"; sleep 30');
 try{
  resumeAt(t,group);
  const r=await runRelease({...t.c,push:{...t.c.push,graceMs:4000,stopMs:300}},t.a);
  assert.deepEqual(pushLog(t),[],"no read and no push while the group runs");assert.equal(groupGone(group.pgid),false);
  assert.deepEqual(r.push,{remote:"origin",commit:t.j.commit,outcome:"failed",reason:"push-failed",remoteCheck:"unknown"});
  assert.equal(t.escalations.length,1);assert.deepEqual(t.escalations[0].pushCause,{reason:"push-failed",remoteCheck:"unknown"});
  const after=readJournalFile(t);assert.deepEqual(after.pushGroup,group,"still journaled");assert.deepEqual(after.pushAttempts.at(-1).earlierGroup,{pgid:group.pgid,proof:"ours",stopped:false});
 }finally{killGroup(group.pgid);}
});
test("push r3 a recorded group that is gone keeps today's resume: no signal, no read before the push",async()=>{
 const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("fail")});rmSync(join(t.dir,"fail"));
 const group=await earlierGroup(t,'touch "$D/ready"; exit 0');await waitFor(()=>groupGone(group.pgid));
 assert.equal(pushGroupState(group),"gone");resumeAt(t,group);
 const r=await runRelease(t.c,t.a);
 assert.deepEqual(pushLog(t),["push"]);assert.deepEqual(r.push,{remote:"origin",commit:t.j.commit,outcome:"pushed"});
 assert.deepEqual(readJournalFile(t).pushAttempts.at(-1).earlierGroup,{pgid:group.pgid,proof:"gone",stopped:true});assert.equal(readJournalFile(t).pushGroup,undefined);
});
test("push r4 a live group the resume cannot prove its own is never signalled",async()=>{
 // Another start time or boot: the pid is someone else's, so the push is over.
 for(const other of [{startedAt:"1970-01-01T00:00:00.000Z"},{boot:"{ sec = 1, usec = 0 }"}]){
  const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("fail")});rmSync(join(t.dir,"fail"));
  const group=await earlierGroup(t,TRAPPED);
  try{
   const recorded={...group,...other};assert.equal(pushGroupState(recorded),"gone");resumeAt(t,recorded);
   const r=await runRelease(t.c,t.a);
   assert.deepEqual(pushLog(t),["push"],"no earlier-term");assert.equal(groupGone(group.pgid),false,"left running");assert.equal(r.push.outcome,"pushed");
  }finally{killGroup(group.pgid);}
 }
 // No proof at all: present, unproven, unsignalled, unread and not pushed again.
 const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("fail")});rmSync(join(t.dir,"fail"));
 const group=await earlierGroup(t,TRAPPED,{proof:false});
 try{
  assert.equal(pushGroupState(group),"unproven");assert.equal(pushGroupState({pgid:1}),"unproven");assert.equal(pushGroupState({pgid:"7"}),"unproven");
  resumeAt(t,group);
  const r=await runRelease(t.c,t.a);
  assert.deepEqual(pushLog(t),[]);assert.equal(groupGone(group.pgid),false);
  assert.deepEqual(r.push,{remote:"origin",commit:t.j.commit,outcome:"failed",reason:"push-failed",remoteCheck:"unknown"});
  assert.deepEqual(readJournalFile(t).pushAttempts.at(-1).earlierGroup,{pgid:group.pgid,proof:"unproven",stopped:false});
 }finally{killGroup(group.pgid);}
});
const ISO_START=/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.000Z$/;
// A fake ps and sysctl first on PATH. In TZ=UTC with LC_ALL=C they run the
// real tool; in any other environment they print a rendering of their own,
// as the tools do in another TZ or locale. as() sets the test process's
// environment, the one an earlier or a resumed runner would have had, and
// rendered() is what the runner read before the fix.
function otherEnvironments(t){
 const dir=mkdtempSync(join(tmpdir(),"runner-fake-ps-")),real=name=>execFileSync("sh",["-c",`command -v ${name} || echo false`],{encoding:"utf8"}).trim();
 const tool=(name,body)=>{writeFileSync(join(dir,name),`#!/bin/sh\nif [ "$TZ" = UTC ] && [ "$LC_ALL" = C ]; then exec ${real(name)} "$@"; fi\n${body}\n`);chmodSync(join(dir,name),0o755);};
 tool("ps",'echo "Mi. 21 Okt. 11:35:50 2026 $TZ"');
 tool("sysctl",`out=$(${real("sysctl")} "$@") || exit 1; echo "\${out%%\\}*}} gerendert $TZ"`);
 const saved=Object.fromEntries(["PATH","TZ","LC_ALL"].map(k=>[k,process.env[k]])),put=(k,v)=>{if(v===undefined)delete process.env[k];else process.env[k]=v;};
 process.env.PATH=`${dir}:${process.env.PATH}`;
 t.after(()=>{for(const [k,v] of Object.entries(saved))put(k,v);rmSync(dir,{recursive:true,force:true});});
 return {as:(tz,lc)=>{put("TZ",tz);put("LC_ALL",lc);},
  rendered:pid=>execFileSync("ps",["-o","lstart=","-p",String(pid)],{encoding:"utf8"}).trim(),
  renderedBoot:()=>{try{return execFileSync("sysctl",["-n","kern.boottime"],{encoding:"utf8",stdio:["ignore","pipe","ignore"]}).trim();}catch{return null;}}};
}
test("push r6 the start and boot identity compare one rule, in any TZ or locale",t=>{
 const now="2026-10-21T18:35:50.000Z",later="2026-10-21T18:35:51.000Z",legacy="Wed Oct 21 11:35:50 2026";
 assert.deepEqual([startMatch(now,now),startMatch(now,later),startMatch(legacy,now),startMatch(now,legacy),startMatch(null,now),startMatch(now,null)],["same","other","unproven","unproven","unproven","unproven"]);
 const macos="{ sec = 1787452403, usec = 399698 }",id="0b3c5d1e-2f40-4a5b-9c6d-7e8f90a1b2c3";
 assert.deepEqual([bootMatch(macos,`${macos} Sat Aug 22 19:33:23 2026`),bootMatch(`${macos} Sun Aug 23 02:33:23 2026`,macos),bootMatch(macos,"{ sec = 1, usec = 0 }"),bootMatch(id,id.toUpperCase()),bootMatch(id,macos),bootMatch("another-boot",macos),bootMatch(undefined,macos)],["same","same","other","same","other","unproven","unproven"]);
 assert.deepEqual([startedBy(now,now,1000),startedBy(later,now,1000),startedBy("2026-10-21T18:35:52.000Z",now,1000),startedBy(legacy,"2030-01-01T00:00:00.000Z",1000),startedBy(null,now,1000)],[true,true,false,false,false]);
 // The same live process reads the same identity whatever the caller's TZ and locale.
 const env=otherEnvironments(t),pid=process.pid;
 env.as("Pacific/Auckland","de_DE.UTF-8");const a=[processStartTime(pid),bootIdentity(),env.rendered(pid),env.renderedBoot()];
 env.as("America/Los_Angeles",undefined);const b=[processStartTime(pid),bootIdentity(),env.rendered(pid),env.renderedBoot()];
 assert.match(a[0],ISO_START);assert.ok(a[1]);assert.deepEqual(a.slice(0,2),b.slice(0,2));
 assert.notEqual(a[2],b[2],"the fake ps renders by environment");
 if(!existsSync("/proc/sys/kernel/random/boot_id"))assert.notEqual(a[3],b[3],"the fake sysctl renders by environment");
});
test("push r6 a live group recorded under another TZ and locale is ours: stopped, read, then pushed",async t0=>{
 const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("fail")});rmSync(join(t.dir,"fail"));
 const env=otherEnvironments(t0);env.as("Pacific/Auckland","de_DE.UTF-8");
 const group=await earlierGroup(t,TRAPPED),then=env.rendered(group.pgid);
 try{
  env.as("America/Los_Angeles",undefined);assert.notEqual(env.rendered(group.pgid),then,"the rendered text differs");
  assert.equal(pushGroupState(group),"ours");resumeAt(t,group);
  const r=await runRelease(t.c,t.a);
  assert.deepEqual(pushLog(t),["earlier-term","ls-remote","push"]);assert.equal(r.push.outcome,"pushed");assert.equal(groupGone(group.pgid),true);
  assert.deepEqual(readJournalFile(t).pushAttempts.at(-1).earlierGroup,{pgid:group.pgid,proof:"ours",stopped:true,remoteCheck:"absent"});
 }finally{killGroup(group.pgid);}
});
test("push r6 a live group journaled before the fix, with rendered text, is unproven: never signalled and never pushed over",async t0=>{
 const t=await pushCase(RESUMABLE,LOGGED_READ,{setup:modes("fail")});rmSync(join(t.dir,"fail"));
 const env=otherEnvironments(t0);env.as("Pacific/Auckland","de_DE.UTF-8");
 const group=await earlierGroup(t,TRAPPED),legacy={...group,startedAt:env.rendered(group.pgid),boot:env.renderedBoot()??group.boot};
 try{
  env.as("America/Los_Angeles",undefined);
  assert.equal(pushGroupState(legacy),"unproven");assert.equal(pushGroupState({...legacy,boot:"another-boot"}),"unproven");
  // Earlier boot text still holds the boot's numbers, so it compares; another boot means the pid is not ours.
  assert.equal(pushGroupState({...group,boot:legacy.boot}),"ours");assert.equal(pushGroupState({...legacy,boot:"{ sec = 1, usec = 0 }"}),"gone");
  resumeAt(t,legacy);
  const r=await runRelease(t.c,t.a);
  assert.deepEqual(pushLog(t),[],"no stop, no read, no push");assert.equal(groupGone(group.pgid),false);
  assert.deepEqual(r.push,{remote:"origin",commit:t.j.commit,outcome:"failed",reason:"push-failed",remoteCheck:"unknown"});
  assert.deepEqual(readJournalFile(t).pushAttempts.at(-1).earlierGroup,{pgid:group.pgid,proof:"unproven",stopped:false});
 }finally{killGroup(group.pgid);}
});
// A runner process with the entry point's stop handling (SIGTERM and SIGINT
// only abort its waits) is stopped mid-push: the push finishes, the release
// completes and the group is cleared before it exits.
test("push r5 a clean runner stop mid-push lets the push finish, and the release completes with its group cleared",async t0=>{
 const gitPath=fakeGit('touch "$D/pushing"; while [ ! -f "$D/go" ]; do sleep 0.05; done'),d=dirname(gitPath),out=join(d,"out.json"),script=join(d,"runner.mjs");
 const url=p=>JSON.stringify(new URL(p,import.meta.url).href);
 writeFileSync(script,`import {readFileSync,writeFileSync} from "node:fs";
import {runRelease} from ${url("../scripts/release-runner.mjs")};
import {fixture,job,change,fake,config} from ${url("./release-runner-fixtures.mjs")};
const [git,out]=process.argv.slice(2),controller=new AbortController();
process.on("SIGTERM",()=>controller.abort());process.on("SIGINT",()=>controller.abort());
const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c={...config(f,j),push:{git}};
writeFileSync(out+".start",JSON.stringify({f,journalPath:c.journalPath,commit:j.commit}));
const r=await runRelease(c,a);
writeFileSync(out,JSON.stringify({r,calls:a.calls,aborted:controller.signal.aborted,journal:JSON.parse(readFileSync(c.journalPath,"utf8"))}));
`);
 const child=spawn(process.execPath,[script,gitPath,out],{stdio:"ignore"}),exited=new Promise(r=>child.once("exit",(code,signal)=>r({code,signal})));
 t0.after(()=>{try{child.kill("SIGKILL");}catch{}});
 let start,during;
 await waitFor(()=>{try{start=JSON.parse(readFileSync(out+".start","utf8"));during=JSON.parse(readFileSync(start.journalPath,"utf8"));return existsSync(join(d,"pushing"))&&!!during.pushGroup;}catch{return false;}},30000);
 assert.equal(during.phase,"pushing");assert.ok(Number.isSafeInteger(during.pushGroup.pgid));
 child.kill("SIGTERM");await new Promise(r=>setTimeout(r,500));
 assert.equal(child.exitCode,null,"the runner still waits on the push");assert.equal(groupGone(during.pushGroup.pgid),false,"the push group is not signalled");
 writeFileSync(join(d,"go"),"");
 assert.deepEqual(await exited,{code:0,signal:null});
 const done=JSON.parse(readFileSync(out,"utf8"));
 assert.equal(done.aborted,true,"the stop reached the runner");assert.equal(done.r.outcome,"released");assert.deepEqual(done.r.push,{remote:"origin",commit:start.commit,outcome:"pushed"});
 assert.equal(done.journal.phase,"complete");assert.equal(done.journal.pushGroup,undefined);assert.ok(done.calls.includes("finish"));assert.equal(remoteHead(start.f),start.commit);
});
test("push p2 the push limit is a named five-minute policy and an invalid commit never runs git",async()=>{
 assert.equal(PUSH_TIMEOUT_MS,300000);const src=readFileSync(new URL("../scripts/release-runner.mjs",import.meta.url),"utf8");assert.ok(!src.includes("timeout:120000"));
 assert.deepEqual(await pushRelease(mkdtempSync(join(tmpdir(),"push-none-")),"not-a-sha",{git:"/nonexistent/git"}),{outcome:"failed",elapsedMs:0,reason:"push-failed"});
});
test("push p1 every push reason tag passes the receipt's reason rule, and a notice drops a malformed cause",()=>{
 for(const tag of PUSH_REASONS)assert.equal(receiptReason(tag),tag,tag);
 const text=pushCauseText({timedOut:"yes",exitCode:"128; rm",signal:"SIGTERM\nfatal",elapsedMs:"9; rm",reason:"token=abc",remoteCheck:"landed\nx",stderrSHA256:"<b>SYNTHETIC_PRIVATE_TOKEN</b>"});
 assert.equal(text,"");assert.equal(pushCauseText(undefined),"");assert.equal(pushCauseText({signal:"sigkill",elapsedMs:1500,reason:"push-killed",remoteCheck:"unknown"})," Cause: signal sigkill after 2 s (push-killed). Remote check: unknown.");
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
test("a Go artifact built for a worktree deployer checkout carries the integrated revision",async t=>{
 // A per-test TMPDIR: another process's build clone in flight is not this one's.
 const parent=mkdtempSync(join(tmpdir(),"release-go-build-")),previousTmpdir=process.env.TMPDIR;process.env.TMPDIR=parent;
 t.after(()=>{if(previousTmpdir===undefined)delete process.env.TMPDIR;else process.env.TMPDIR=previousTmpdir;rmSync(parent,{recursive:true,force:true});});
 const {commit,deployer,adapter}=goWorktreeFixture();assert.ok(statSync(join(deployer,".git")).isFile(),"the deployer checkout is a worktree");
 const artifact=await adapter.prepare("mini",commit);const info=execFileSync("go",["version","-m",artifact.artifactPath],{encoding:"utf8"});
 assert.match(info,new RegExp(`\\tbuild\\tvcs\\.revision=${commit}\\n`));assert.match(info,/\tbuild\tvcs\.modified=false\n/);
 assert.equal(readdirSync(parent).filter(d=>d.startsWith("tailterm-release-build-")).filter(d=>existsSync(join(parent,d,"src"))).length,0,"the build clone is removed");
});
test("an unstamped or mismatched Go artifact is refused before any deploy",async()=>{
 for(const reply of [c=>"x: go1.26\n",c=>stamped("f".repeat(40)),c=>stamped(c,"true")]){
  const {commit,adapter}=goWorktreeFixture();const real=adapter.command.bind(adapter);adapter.command=(argv,cwd)=>argv[1]==="version"?reply(commit):real(argv,cwd);
  await assert.rejects(adapter.prepare("mini",commit),/build revision|does not match/);assert.equal(adapter.artifacts.size,0);
 }
});
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
test("a3 the host adapter refuses unpaired hub and bridge inputs before publication",async()=>{
 const h=pairedHost();h.importTargets({hub:{...h.shared,planTargets:["hub"],planPath:"/p/hub"},bridge:{...h.shared,planTargets:["bridge"],planPath:"/p/bridge"}});
 assert.throws(()=>h.adapter.jobInputs(h.commit),/one paired plan/);
 h.importTargets({hub:h.shared,bridge:{...h.shared,backupSHA256:"e".repeat(64)}});assert.throws(()=>h.adapter.jobInputs(h.commit),/one paired plan/);
 h.importTargets({hub:h.shared,bridge:h.shared});assert.equal(h.adapter.jobInputs(h.commit).targets.bridge.release,h.shared.release);
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake();a.verifyInputs=async()=>{throw releaseError("Hub and bridge need one paired plan");};
 await assert.rejects(runRelease(config(f,j),a),/refused/);assert.deepEqual(a.calls,["refuse","escalate"]);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);
});
test("p1 p2 a lone hub or bridge entry that names the paired plan is refused before publication",async()=>{
 const h=pairedHost();
 for(const lone of ["hub","bridge"]){h.importTargets({[lone]:h.shared});assert.throws(()=>h.adapter.jobInputs(h.commit),/^Error: Paired plan partner entry required$/,lone);}
 // The real input check, reached through runRelease before publishIntegration.
 h.importTargets({hub:h.shared});
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);a.verifyInputs=async()=>{h.adapter.jobInputs(h.commit);return true;};
 await assert.rejects(runRelease(c,a),/refused/);
 assert.deepEqual(a.calls,["refuse","escalate"],"no merged call and no deploy");assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);assert.equal(remoteHead(f),f.base);
 const journal=JSON.parse(readFileSync(c.journalPath,"utf8"));assert.equal(journal.refusalReason,"Paired plan partner entry required");assert.notEqual(journal.published,true);
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
test("p1 provisioning installs and copies each missing prerequisite, never overwrites and is idempotent",()=>{
 const p=provisionFixture();mkdirSync(join(p.cwd,".build"));writeFileSync(join(p.cwd,".build/test.wasm"),"local build");
 const first=provisionPrerequisites(p.cwd,{from:p.from,run:p.run});
 assert.deepEqual(p.installs,[[["npm","ci"],p.cwd]]);
 assert.deepEqual(first.prerequisites.map(x=>[x.path,x.action]),[["node_modules/.package-lock.json","installed"],["wasm/tailserve.wasm","copied"],[".build/test.wasm","present"],[".build/speech-fixture.wav","copied"],[".build/go-modules.txt","copied"]]);
 assert.equal(readFileSync(join(p.cwd,".build/test.wasm"),"utf8"),"local build");assert.equal(readFileSync(join(p.cwd,"wasm/tailserve.wasm"),"utf8"),"fixture wasm/tailserve.wasm");
 for(const x of first.prerequisites)assert.equal(x.sha256,hash(readFileSync(join(p.cwd,x.path))));
 for(const x of first.prerequisites)if(x.action==="copied")assert.deepEqual([Object.keys(x).sort(),x.sourceCommit],[["action","path","sha256","sourceCommit"],p.commit],x.path);else assert.deepEqual(Object.keys(x).sort(),["action","path","sha256"],x.path);
 assert.equal(git(p.cwd,"status","--porcelain"),"");
 const again=provisionPrerequisites(p.cwd,{from:p.from,run:p.run});assert.equal(p.installs.length,1);assert.ok(again.prerequisites.every(x=>x.action==="present"&&!("sourceCommit" in x)));
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
// wi_adf23937db2c4d0c: a copy needs the source's commit; without one nothing is installed or copied.
test("p1 provisioning refuses a source whose commit cannot be read before changing anything, and reads none when nothing is copied",()=>{
 const reason="Prerequisite source commit unreadable: git rev-parse HEAD failed";
 const plain=()=>{const from=mkdtempSync(join(tmpdir(),"provision-plain-"));placePrerequisites(from);return from;};
 const unborn=()=>{const from=plain();git(from,"init","-q");return from;};
 for(const from of [plain(),unborn()]){
  const p=provisionFixture();
  assert.throws(()=>provisionPrerequisites(p.cwd,{from,run:p.run}),e=>failureReason(e)===reason);
  assert.deepEqual(p.installs,[],"npm ci never ran");
  for(const x of MATRIX_PREREQUISITES)assert.ok(!existsSync(join(p.cwd,x)),x+" not copied");
  assert.equal(git(p.cwd,"status","--porcelain"),"");
 }
 // Everything present, or only the install marker missing: no copy, so no commit is read.
 const done=provisionFixture();placePrerequisites(done.cwd);
 const present=provisionPrerequisites(done.cwd,{from:plain(),run:done.run});
 assert.ok(present.prerequisites.every(x=>x.action==="present"&&!("sourceCommit" in x)));
 const marker=provisionFixture();placePrerequisites(marker.cwd);rmSync(join(marker.cwd,"node_modules/.package-lock.json"));
 const installed=provisionPrerequisites(marker.cwd,{from:unborn(),run:marker.run});
 assert.deepEqual(installed.prerequisites.map(x=>x.action),MATRIX_PREREQUISITES.map((x,i)=>i?"present":"installed"));assert.ok(installed.prerequisites.every(x=>!("sourceCommit" in x)));
 const c=provisionFixture();
 const r=spawnSync(process.execPath,[RUNNER,"--provision-prerequisites","--from",plain()],{cwd:c.cwd,encoding:"utf8"});
 assert.deepEqual({status:r.status,stdout:r.stdout,stderr:r.stderr},{status:1,stdout:"",stderr:reason+"\n"});
 assert.deepEqual(c.installs,[]);assert.equal(git(c.cwd,"status","--porcelain"),"");
});
test("p1 the provisioning command prints the prerequisites, or only the named reason",()=>{
 const p=provisionFixture();mkdirSync(join(p.cwd,"node_modules"));writeFileSync(join(p.cwd,"node_modules/.package-lock.json"),"{}");
 const out=JSON.parse(execFileSync(process.execPath,[RUNNER,"--provision-prerequisites","--from",p.from],{cwd:p.cwd,encoding:"utf8"}));
 assert.equal(out.version,1);assert.deepEqual(out.prerequisites.map(x=>x.action),["present","copied","copied","copied","copied"]);assert.equal(git(p.cwd,"status","--porcelain"),"");
 assert.deepEqual(out.prerequisites.map(x=>x.sourceCommit),[undefined,p.commit,p.commit,p.commit,p.commit]);
 const q=provisionFixture();mkdirSync(join(q.cwd,"node_modules"));writeFileSync(join(q.cwd,"node_modules/.package-lock.json"),"{}");rmSync(join(q.from,".build/go-modules.txt"));
 const r=execFileSync(process.execPath,["-e",`const r=require("child_process").spawnSync(process.execPath,${JSON.stringify([RUNNER,"--provision-prerequisites","--from",q.from])},{cwd:${JSON.stringify(q.cwd)},encoding:"utf8"});console.log(JSON.stringify({status:r.status,stdout:r.stdout,stderr:r.stderr}))`],{encoding:"utf8"});
 assert.deepEqual(JSON.parse(r),{status:1,stdout:"",stderr:"Missing matrix prerequisites: .build/go-modules.txt\n"});
});
