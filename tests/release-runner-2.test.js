import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,readdirSync,rmSync,symlinkSync,renameSync,utimesSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname} from "node:path";
import {execFileSync,spawn,spawnSync} from "node:child_process";
import {integrateCandidate,publishIntegration,runRelease,liveCheck,runnableJob,HostAdapter,serveDeployment,hostLockNames,retentionPolicy,pruneJournal,rehearsalArgv,failureRehearsal,reconcileHostLocks,revertCommit,moveReleaseRef,tasksHubCheckedOut,releaseError,failureReason,receiptReason,probeDiagnostics,withoutDiagnostics,sameReceipt,validateNativeRelease,reconcileReceipts,MATRIX_PREREQUISITES,MATRIX_HOST_WAIT_MS,MATRIX_LAUNCH_GRACE_MS,MATRIX_STOP_GRACE_MS,MATRIX_DEADLINE_SLACK_MS,missingPrerequisites,provisionPrerequisites,matrixRunTimeout,matrixPriority,matrixWaitNotice,matrixHeldNotice,fenceWaitNotice,matrixRunUnsettled} from "../scripts/release-runner.mjs";
import {acquireHostLock,readJournal,updateHostState,pidGone,groupGone,RUN_TIMEOUT_GRACE_MS,DEFAULT_HOLDER_CAP_MS} from "../scripts/verify-matrix-host-lock.mjs";
import {planRunTimeout,readPrerequisites} from "../scripts/verify-matrix.mjs";
import {createHash} from "node:crypto";
import {renderTeamDelivery} from "../client/team-delivery-view.js";
import {targetsForPaths,selectReleaseTargets} from "../scripts/release-targets.mjs";
import {releaseReply,releaseReplySource,readHostState,git,bufferTT,fixture,change,job,fake,config,miniFixture,hash,stamped,importInputs,hostFixture,manifestFile,reclaimed,otherGeneration,HUB_SERVICE,rehearsalFixture,REPO,composePlan,recorded,schemaRelease,journalFixture,copyOf,sweep,C,DIST,SWEEP_AT,tailosJob,fiveReleased,addDist,distFixture,sweepAt,distRecord,addAttempt,EVIDENCE,BULK,YOUNG,attemptRecord,bulkRecords,remoteHead,goWorktreeFixture,PAIRED,pairedRelease,pairedHost,RUNNER,ignorePrerequisites,placePrerequisites,workedPlan,exitedPid,attemptDir,matrixHost,provisionFixture,until,ended,idle,lockHost,holderEntry,otherHolder,reasonIs,releaseHost,tailosAdapter,MATRIX_A,MATRIX_B,uncoveredReason,changedMatrixHost,matrixChangeRelease,heldRun,three,race,covers,stale,claimed} from "./release-runner-fixtures.mjs";
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
  writeFileSync(manifest,"{}");assert.throws(()=>adapter.jobInputs(commit),/another generation/);
 }
});
test("the runner reads the manifest the hub bound for this attempt among those of earlier generations",()=>{
 const {commit,adapter,dir,bind}=reclaimed(),first=manifestFile(adapter,commit,"rel_sel-g3-inputs.json",{generation:3,attempt:0}),second=manifestFile(adapter,commit,"rel_sel-g8-inputs.json",{generation:8,attempt:1});
 manifestFile(adapter,commit,"rel_selx-g8-inputs.json",{generation:8,attempt:1});bind(second);
 const input=adapter.jobInputs(commit);assert.deepEqual([input.generation,input.attempt],[8,1]);assert.deepEqual(input,JSON.parse(second.raw));
 assert.equal(readFileSync(first.path,"utf8"),first.raw,"the earlier attempt's manifest is only read");assert.deepEqual(readdirSync(dir).sort(),["rel_sel-g3-inputs.json","rel_sel-g8-inputs.json","rel_selx-g8-inputs.json"]);
 // The inputs command's copy under the name without a generation holds the
 // same bytes; the generation-named file is still the one checked and read.
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
 // Only an earlier attempt's manifest exists; the bound one is not there.
 let x=reclaimed();manifestFile(x.adapter,x.commit,"rel_sel-g3-inputs.json",{generation:3,attempt:0});x.adapter.job.inputsDigest="e".repeat(64);
 assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration);
 // The file named for generation 8 records generation 3.
 x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:3,attempt:1}));assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration);
 // The recorded generation is not below the job's: the import always moves the job on.
 for(const g of [9,12]){x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,`rel_sel-g${g}-inputs.json`,{generation:g,attempt:1}));assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration,"generation "+g);}
 // The recorded attempt is not this one.
 for(const attempt of [0,2,undefined]){x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:8,attempt}));assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration,"attempt "+attempt);}
 // With no manifest of any generation the binding itself is what is missing.
 x=reclaimed();x.adapter.job.inputsDigest="e".repeat(64);assert.throws(()=>x.adapter.jobInputs(x.commit),e=>failureReason(e)==="Handler input digest binding required");
});
test("a manifest written before names carried a generation is read only when it is the bound one",()=>{
 let x=reclaimed();const old=manifestFile(x.adapter,x.commit,"rel_sel-inputs.json",{});x.bind(old);
 assert.deepEqual(x.adapter.jobInputs(x.commit),JSON.parse(old.raw));assert.equal(x.adapter.backupKey(JSON.parse(old.raw)),"rel_sel");
 // The old name with a generation field is not a manifest of that time.
 x=reclaimed();x.bind(manifestFile(x.adapter,x.commit,"rel_sel-inputs.json",{generation:8,attempt:1}));assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration);
 // Another digest: not the bound manifest, with or without a later attempt's file beside it.
 x=reclaimed();manifestFile(x.adapter,x.commit,"rel_sel-inputs.json",{});x.adapter.job.inputsDigest="e".repeat(64);
 assert.throws(()=>x.adapter.jobInputs(x.commit),e=>failureReason(e)==="Handler input digest binding required");
 manifestFile(x.adapter,x.commit,"rel_sel-g8-inputs.json",{generation:8,attempt:1});assert.throws(()=>x.adapter.jobInputs(x.commit),otherGeneration);
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
test("a14 the integrated run is started with TAILTERM_MATRIX_RELEASE=1 in its environment and its argv unchanged",async t=>{
 const h=matrixHost(),dir=attemptDir(h.home,h.integrated.integratedCommit,0);let given;h.adapter.hostState=()=>null;
 delete h.adapter.startMatrixRun;const start=h.adapter.startMatrixRun.bind(h.adapter);
 h.adapter.startMatrixRun=(argv,d)=>{given=argv;return start([process.execPath,"-e","console.log(JSON.stringify([process.env.TAILTERM_MATRIX_RELEASE,process.argv.slice(1)]));setInterval(()=>{},1000)",...argv.slice(1)],d);};
 const inherited=process.env.TAILTERM_MATRIX_RELEASE;delete process.env.TAILTERM_MATRIX_RELEASE;t.after(()=>{if(inherited!==undefined)process.env.TAILTERM_MATRIX_RELEASE=inherited;});
 assert.equal(await h.adapter.verifyIntegrated({...h.integrated,itemId:"wi_0123456789abcdef"}),false);
 const child=h.adapter.matrixChildren.get(dir);t.after(()=>{try{process.kill(-child.pid,"SIGKILL");}catch{}});
 assert.deepEqual(given,["node","scripts/verify-matrix.mjs","run",join(dir,"plan.json"),dir,"--priority","high","--item","wi_0123456789abcdef","--host-wait-minutes","120"],"no marker flag in argv");
 await until(()=>readFileSync(join(dir,"run.out"),"utf8").includes("\n"),"the run's output on file");
 assert.deepEqual(JSON.parse(readFileSync(join(dir,"run.out"),"utf8")),["1",given.slice(1)]);
 assert.equal(process.env.TAILTERM_MATRIX_RELEASE,undefined,"the runner's own environment is not marked");
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
test("R4 a failed TailOS release journals both waits, escalates them and sends them in the receipt without nulls",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let receipt,escalation;
 const live={lastCommit:"c".repeat(40),waitedMs:90000},back={lastCommit:null,waitedMs:90000};
 a.check=async()=>"identity";a.probeWait=(t,kind)=>t==="tailos"?(kind==="live"?live:back):undefined;a.finish=async r=>{receipt=r;a.calls.push("finish");};a.escalate=async d=>{escalation=d;a.calls.push("escalate");};
 await assert.rejects(runRelease(c,a),/inspect saved journal/);
 const journal=JSON.parse(readFileSync(c.journalPath,"utf8")),effect=journal.effects.find(e=>e.target==="tailos");
 assert.deepEqual(effect.liveCheck,live);assert.deepEqual(effect.rollbackCheck,back);
 assert.deepEqual(escalation.probeWaits,[{target:"tailos",probe:"live",...live},{target:"tailos",probe:"rollback",...back}]);
 assert.deepEqual(Object.keys(receipt).sort(),["commit","failure","jobId","outcome","revert","targets","verificationDigest","version"]);
 assert.deepEqual(receipt.failure,{step:"live-check",target:"tailos",reason:"live verification failed"});
 assert.deepEqual(receipt.targets.map(t=>Object.keys(t).sort()),[["artifactSHA256","liveCheck","outcome","reason","release","rollback","rollbackCheck","target"]]);
 assert.equal(receipt.targets[0].reason,"live verification failed");
 assert.deepEqual(receipt.targets[0].liveCheck,{lastCommit:"c".repeat(40),waitedMs:90000});assert.deepEqual(receipt.targets[0].rollbackCheck,{waitedMs:90000});
 assert.ok(!JSON.stringify(receipt).includes("null"));assert.deepEqual(journal.receipt,receipt,"the journal keeps the receipt a retried finish resends");
});
// wi_d4e96831ab3a0038: what a failed receipt carries, and what never reaches it.
test("D1 a hub failure's receipt carries only the probe wait's time, never its polls or capture",async()=>{
 const p=pairedRelease(),SECRET="SYNTHETIC_PRIVATE_TOKEN",wait={waitedMs:240000,polls:49,capture:{app:{state:SECRET},logs:[{service:"hub",lines:[SECRET]}]}};
 p.a.check=async t=>t==="hub"?"identity":true;p.a.probeWait=t=>t==="hub"?wait:undefined;
 await assert.rejects(runRelease(p.c,p.a),/failed/);
 const r=p.receipt(),hub=r.targets.find(t=>t.target==="hub"),bridge=r.targets.find(t=>t.target==="bridge");
 assert.deepEqual(r.failure,{step:"live-check",target:"hub",reason:"live verification failed"});
 assert.equal(hub.reason,"live verification failed");assert.deepEqual(hub.liveCheck,{waitedMs:240000});assert.deepEqual(hub.rollbackCheck,{waitedMs:240000});
 assert.equal(bridge.reason,undefined);assert.equal(bridge.liveCheck,undefined);
 const text=JSON.stringify(r);for(const word of [SECRET,"polls","capture"])assert.ok(!text.includes(word),word);
 assert.deepEqual(p.journal().effects.find(e=>e.target==="hub").liveCheck,wait,"the private journal keeps the capture");
});
test("D2 a secret-looking failure tag is sent as unclassified and never reaches the receipt",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let receipt;
 a.deploy=async()=>{throw releaseError("SYNTHETIC_PRIVATE_TOKEN");};a.finish=async r=>{receipt=r;a.calls.push("finish");};
 await assert.rejects(runRelease(c,a),/inspect saved journal/);
 assert.deepEqual(receipt.failure,{step:"deploy",target:"tailos",reason:"unclassified"});assert.equal(receipt.targets[0].reason,"unclassified");
 assert.ok(!JSON.stringify(receipt).includes("SYNTHETIC_PRIVATE_TOKEN"));
 for(const bad of ["Bearer SYNTHETIC_PRIVATE_VALUE","password: x","an API key","key=value","a\nb","x".repeat(161),"",null,7])assert.equal(receiptReason(bad),"unclassified",String(bad));
 assert.deepEqual(probeDiagnostics({lastCommit:null,waitedMs:null,polls:3}),undefined);assert.deepEqual(probeDiagnostics({lastCommit:"C".repeat(40),waitedMs:-1}),undefined);
 assert.deepEqual(probeDiagnostics({lastCommit:"c".repeat(40),waitedMs:0,capture:{}}),{lastCommit:"c".repeat(40),waitedMs:0});assert.deepEqual(probeDiagnostics({waitedMs:3600001}),undefined);
});
test("D3 a released receipt keeps exactly today's keys, whatever its probes waited",async()=>{
 const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);let receipt;
 a.probeWait=()=>({lastCommit:"c".repeat(40),waitedMs:9000});a.finish=async r=>{receipt=r;a.calls.push("finish");};
 await runRelease(c,a);
 assert.equal(receipt.outcome,"released");assert.deepEqual(Object.keys(receipt).sort(),["commit","jobId","outcome","push","targets","verificationDigest","version"]);
 assert.deepEqual(receipt.targets.map(t=>Object.keys(t).sort()),[["artifactSHA256","outcome","release","target"]]);
});
test("D4 every fixed release tag passes the receipt's reason rule unchanged, so the hub never refuses one",()=>{
 const scripts=readdirSync(join(REPO,"scripts")).filter(n=>/^release-.*\.mjs$/.test(n)),tags=new Set();
 for(const name of scripts)for(const m of readFileSync(join(REPO,"scripts",name),"utf8").matchAll(/releaseError\(("(?:[^"\\]|\\.)*"|`[^`$]*`)/g))tags.add(m[1].startsWith("`")?m[1].slice(1,-1):JSON.parse(m[1]));
 tags.add(uncoveredReason);
 assert.ok(tags.size>50,String(tags.size));
 for(const tag of tags)assert.equal(receiptReason(tag),tag,tag);
});
test("D5 native finish and reconcile compare the receipt sent, or it without diagnostics when the hub saved none",()=>{
 const commit="e".repeat(40),old={version:1,jobId:"rel_fixture",commit,verificationDigest:"f".repeat(64),outcome:"rolled_back",revert:{outcome:"failed"},targets:[{target:"tailos",release:"tailos-x",artifactSHA256:"a".repeat(64),outcome:"rolled_back",rollback:"restored"}]};
 const diagnosed={...old,failure:{step:"live-check",target:"tailos",reason:"live verification failed"},targets:[{...old.targets[0],reason:"live verification failed",liveCheck:{lastCommit:"c".repeat(40),waitedMs:90000},rollbackCheck:{waitedMs:0}}]};
 const other={...diagnosed,targets:[{...diagnosed.targets[0],rollbackCheck:{waitedMs:1}}]},otherIdentity={...old,targets:[{...old.targets[0],artifactSHA256:"b".repeat(64)}]};
 assert.deepEqual(withoutDiagnostics(diagnosed),old);
 const prior={id:"rel_fixture",taskId:"tsk_fixture",entryId:"tqe_fixture",itemId:"wi_fixture",itemRevision:1,scopeRevision:1,orderMessageSeq:1,repository:"r",baseCommit:"b".repeat(40),commit:"c".repeat(40),verificationDigest:"f".repeat(64),pauseGeneration:1,state:"merged",generation:2,agentId:"agt_fixture",runId:"run_fixture",integratedCommit:commit,published:true};
 const finish=(sent,saved)=>validateNativeRelease(prior,{...prior,state:"rolled_back",generation:3,receipt:saved},"finish",2,[],sent);
 const home=mkdtempSync(join(tmpdir(),"receipt-compare-")),reconcile=(sent,saved)=>{
  writeFileSync(join(home,"rel_fixture.json"),JSON.stringify({jobId:"rel_fixture",commit:prior.commit,agentId:prior.agentId,runId:prior.runId,phase:"receipt_pending",finishGeneration:2,receipt:sent}));
  reconcileReceipts({journalDirectory:home},[{...prior,state:"rolled_back",generation:3,receipt:saved}]);
  return JSON.parse(readFileSync(join(home,"rel_fixture.json"),"utf8")).phase;
 };
 for(const [sent,saved] of [[old,old],[diagnosed,diagnosed],[diagnosed,old]]){assert.equal(finish(sent,saved).receipt,saved);assert.equal(reconcile(sent,saved),"complete");}
 assert.ok(sameReceipt(diagnosed,structuredClone(diagnosed)));assert.ok(!sameReceipt(old,diagnosed),"a saved diagnostic never matches a receipt sent without it");
 for(const [sent,saved] of [[diagnosed,other],[old,diagnosed],[diagnosed,otherIdentity],[old,otherIdentity]]){
  assert.throws(()=>finish(sent,saved),/Native full receipt mismatch/);assert.throws(()=>reconcile(sent,saved),/Saved release receipt does not match pending journal/);
 }
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

// Target selection before the push, and the candidate's own map (wi_fd1a6890a531e2c1).
const NEW_FILE="tools/newthing/a.js",NEW_REASON="Unknown release path: tools/newthing/a.js";
// A stand-in for a candidate's scripts/release-targets.mjs that knows the new folder; no imports.
const NEW_RULE=`export function targetsForPaths(paths){for(const p of paths)if(!/^tools\\/newthing\\//.test(p))throw new Error("Unknown release path");return [];}\n`;
// A candidate that adds a new folder, with or without the map rule for it.
function newFolder(withRule){
 const f=fixture();mkdirSync(join(f.cwd,"tools/newthing"),{recursive:true});
 if(withRule){mkdirSync(join(f.cwd,"scripts"));writeFileSync(join(f.cwd,"scripts/release-targets.mjs"),NEW_RULE);}
 const j=job(f,change(f,NEW_FILE,"a")),a=fake(),c=config(f,j),said=[];
 a.prepare=async t=>["hub","bridge"].includes(t)?{...PAIRED}:{release:"rel_fixture-"+t,artifactSHA256:"b".repeat(64)};
 a.escalate=async details=>{said.push(details);a.calls.push("escalate");};
 for(const gate of ["verifyIntegrated","verifyInputs"])a[gate]=async()=>{a.calls.push(gate);return true;};
 return {f,j,a,c,said,journal:()=>JSON.parse(readFileSync(c.journalPath,"utf8"))};
}
test("r1 a candidate that adds a new folder with its map rule releases in one step",async()=>{
 const n=newFolder(true),r=await runRelease(n.c,n.a);
 assert.equal(r.outcome,"released");assert.deepEqual(r.targets.map(t=>[t.target,t.outcome]),["hub","bridge","mini","tailos"].map(t=>[t,"released"]),"the changed map file selects every target");
 assert.deepEqual(n.a.calls,["verifyInputs","merged","deploy:hub","deploy:mini","deploy:tailos","finish"],"no rollback, escalation, bug request or refusal");
 const journal=n.journal();assert.equal(journal.published,true);assert.equal(journal.revert,undefined);assert.equal(journal.failure,undefined);assert.equal(journal.phase,"complete");
 assert.equal(git(n.f.cwd,"rev-parse","refs/heads/tasks-hub"),n.j.commit);assert.equal(remoteHead(n.f),n.j.commit);
});
test("r2 a candidate that adds a new folder without a rule is refused unpublished with the tagged reason",async()=>{
 const n=newFolder(false);await assert.rejects(runRelease(n.c,n.a),/refused/);
 assert.deepEqual(n.a.calls,["refuse","escalate"],"no merged, deploy, rollback or bug request");
 assert.deepEqual(n.said,[{jobId:"rel_fixture",outcome:"refused",reason:NEW_REASON}]);
 const journal=n.journal();assert.notEqual(journal.published,true);assert.deepEqual(journal.effects,[]);assert.equal(journal.revert,undefined);assert.equal(journal.receipt,undefined);
 assert.equal(journal.phase,"refused");assert.equal(journal.refusalReason,NEW_REASON);assert.equal(journal.failureDetail.text,NEW_REASON);assert.deepEqual(journal.failure,{step:"select",reason:NEW_REASON});
 assert.equal(git(n.f.cwd,"rev-parse","refs/heads/tasks-hub"),n.f.base);assert.equal(remoteHead(n.f),n.f.base);
 assert.equal(git(n.f.cwd,"log","--format=%s",n.f.base+"..refs/heads/tasks-hub"),"","no revert commit");
});
test("r3 the refusal notice names the unknown path",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"escalate-adapter-")),calls=[];const adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate({jobId:"rel_fixture",outcome:"refused",reason:NEW_REASON});
 assert.equal(calls.length,1);const field=n=>calls[0][calls[0].indexOf(n)+1];
 assert.equal(field("--subject"),"Release refused before publication");assert.ok(field("--text").includes("Reason: Unknown release path: tools/newthing/a.js."));assert.ok(field("--text").includes("nothing was published or deployed"));
 for(const reason of ["Unknown release path: tools/newthing/a.js and 3 more","Unknown release path (2 paths, names withheld)"]){await adapter.escalate({outcome:"refused",reason});assert.ok(calls.at(-1)[calls.at(-1).indexOf("--text")+1].includes(`Reason: ${reason}.`),reason);}
});
test("r4 targets are selected before the integrated matrix run, the inputs request and the push",async()=>{
 // tasks-hub moved, so the integrated commit differs and a selected job would ask for its matrix run.
 const refused=newFolder(false);git(refused.f.cwd,"checkout","tasks-hub");const tip=change(refused.f,"client/c.js","c");
 await assert.rejects(runRelease(refused.c,refused.a),/refused/);
 assert.deepEqual(refused.a.calls,["refuse","escalate"],"neither verifyIntegrated nor verifyInputs ran");
 assert.deepEqual(refused.journal().failure,{step:"select",reason:NEW_REASON});assert.notEqual(refused.journal().published,true);
 assert.equal(git(refused.f.cwd,"rev-parse","refs/heads/tasks-hub"),tip);assert.equal(remoteHead(refused.f),refused.f.base);
 // The same candidate with its rule: selection passes on the integrated commit, then the gates and the push run in order.
 const n=newFolder(true);git(n.f.cwd,"checkout","tasks-hub");change(n.f,"client/c.js","c");
 assert.equal((await runRelease(n.c,n.a)).outcome,"released");
 assert.deepEqual(n.a.calls,["verifyIntegrated","verifyInputs","merged","deploy:hub","deploy:mini","deploy:tailos","finish"]);
 assert.notEqual(n.journal().integrated,n.j.commit);assert.equal(remoteHead(n.f),n.journal().integrated);
});
