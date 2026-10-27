import test from "node:test";
import {createServer} from "node:http";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,copyFileSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,readdirSync,rmSync,realpathSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname,resolve} from "node:path";
import {execFileSync,execFile,spawnSync} from "node:child_process";
import {refusalText,heldClaimNotice,integrateCandidate,integrateBatch,NO_CHANGE,publishIntegration,revertCommit,moveReleaseRef,runRelease,HostAdapter,serveDeployment,releaseError,compatibilityArgv,dispatchCompatibility,validateNativeRelease,failureReason,failureDetail,matrixWaitNotice,RUNNER_CODE_FILES,LOADED_CODE,CODE_REASONS,codeDigest,diskCode,publishedCode,codeDecision,prepareCode,restoreCode,runnerCodeGate,codeRecord,codeNotice,cliFailureNotice,cliRetry,transportWait,transportWaitNotice,heldRetry,journalIdle,runnerLoads} from "../scripts/release-runner.mjs";
import {acquireHostLock,readHostState as rawReadHostState,holdersOf} from "../scripts/verify-matrix-host-lock.mjs";
import {createHash} from "node:crypto";
import {RELEASE_READ_ATTEMPTS,wranglerBin} from "../scripts/release-inputs.mjs";
import {releaseReply,releaseReplySource,git,bufferTT,fixture,change,job,fake,config,hash,stamped,importInputs,hostFixture,recorded,C,remoteHead,RUNNER,attemptDir,until,ended,idle,lockHost,reasonIs,MATRIX_A,uncoveredReason,matrixChangeRelease,READY_ID,READY_UP,READY_CAPTURE,READY_DOWN,readyAdapter,EXIT1,countedLiveCheck,readyRelease,TOKEN,NEXT_ACTION,conflictFixture,journalOf,refusesUnchanged,checkoutState,CODE_AGENT,CODE_RUN,SAFE_TEXT,codeRepo,codeHost,verifiedJob,codeGate,codeSends,HELD_ITEM,HELD_SECRET,HELD_HANDLER,HELD_HELPER,heldRoster,sendOf,heldRun,unwatchedImports,heldOwn,heldJournal,HELD_SUBJECT,three,race,covers,planned,stale,claimed,LIST_FAILS,failList,codeJournal,taskLock,codeServe,codeCommit,unloadable} from "./release-runner-fixtures.mjs";
test("m5 end to end: a matrix change no owner approval covers refuses the job by name and leaves tasks-hub alone",async()=>{
 const r=matrixChangeRelease([{digest:hash(MATRIX_A),messageSeq:11}]);
 await assert.rejects(runRelease(r.c,r.a),/Release refused before publication/);
 assert.deepEqual(r.a.calls,["refuse","escalate"]);assert.equal(r.matrix().length,0);
 const journal=JSON.parse(readFileSync(r.c.journalPath,"utf8"));assert.equal(journal.phase,"refused");assert.equal(journal.refusalReason,uncoveredReason);assert.notEqual(journal.published,true);assert.deepEqual(journal.effects,[]);
 const sends=r.sends();assert.equal(sends.length,1);assert.equal(sends[0][sends[0].indexOf("--subject")+1],"Release refused before publication");
 const text=sends[0][sends[0].indexOf("--text")+1];assert.ok(text.includes(`Reason: ${uncoveredReason}.`));assert.doesNotMatch(text,/verify-matrix\.mjs exit/);
 assert.equal(git(r.f.cwd,"rev-parse","refs/heads/tasks-hub"),r.tip);assert.equal(remoteHead(r.f),r.originHead);
});
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
// An accepted no-change result (wi_d46b5de7ad79c8c3): a commit already on an
// advanced tasks-hub integrates as nothing, and only a merge commit is nonlinear.
const advanced=f=>{git(f.cwd,"checkout","tasks-hub");const tip=change(f,"client/c.js","c");return tip;};
const noPick=(cwd,tip,label)=>{assert.deepEqual(checkoutState(cwd),{status:"",head:tip,ref:tip},label);assert.equal(git(cwd,"rev-list","--count","HEAD"),"2",label);};
test("n1 a commit equal to its base, or already on tasks-hub, integrates with no cherry-pick and no changed paths",()=>{
 const e=fixture(),tip=advanced(e);
 assert.deepEqual(integrateCandidate(e.cwd,job(e,e.base)),{expected:tip,integrated:tip,changed:[],noChange:NO_CHANGE});noPick(e.cwd,tip,"commit equals base");
 // The candidate's own commit is already an ancestor of the advanced tip.
 const a=fixture(),ac=change(a,"client/a.js","a");git(a.cwd,"checkout","tasks-hub");git(a.cwd,"merge","--ff-only","candidate");const atip=change(a,"client/c.js","c");
 assert.deepEqual(integrateCandidate(a.cwd,job(a,ac)),{expected:atip,integrated:atip,changed:[],noChange:NO_CHANGE});
 assert.deepEqual(checkoutState(a.cwd),{status:"",head:atip,ref:atip});assert.equal(git(a.cwd,"rev-list","--count","HEAD"),"3");
 // A tip that has not moved still fast-forwards as before, with no reason.
 const s=fixture();git(s.cwd,"checkout","tasks-hub");assert.deepEqual(integrateCandidate(s.cwd,job(s,s.base)),{expected:s.base,integrated:s.base,changed:[]});
});
test("n1 a no-change job settles released with nothing verified, published, deployed or pushed, and its journal says why",async()=>{
 const f=fixture(),tip=advanced(f),origin=remoteHead(f),j=job(f,f.base),a=fake(),c=config(f,j);
 a.merged=async commit=>a.calls.push("merged:"+commit);a.verifyIntegrated=async()=>{a.calls.push("verify");return true;};a.verifyInputs=async()=>{a.calls.push("inputs");return true;};
 a.finish=async(r,generation)=>a.calls.push("finish:"+JSON.stringify(r));
 const receipt=await runRelease(c,a),expected={version:1,jobId:"rel_fixture",commit:f.base,verificationDigest:"a".repeat(64),targets:[],outcome:"released"};
 assert.deepEqual(receipt,expected);assert.deepEqual(a.calls,["merged:"+f.base,"finish:"+JSON.stringify(expected)]);
 const journal=JSON.parse(readFileSync(c.journalPath,"utf8"));
 assert.equal(journal.phase,"complete");assert.equal(journal.noChange,NO_CHANGE);assert.notEqual(journal.published,true);assert.deepEqual(journal.effects,[]);
 assert.equal(journal.expected,tip);assert.equal(journal.integrated,tip);assert.deepEqual(journal.receipt,expected);
 assert.deepEqual(checkoutState(f.cwd),{status:"",head:tip,ref:tip});assert.equal(remoteHead(f),origin);
 // A batch lead with no change opens no batch and settles the same way.
 const b=fixture(),btip=advanced(b);git(b.cwd,"checkout","candidate");const mc=change(b,"client/m.js","m");
 const lead=job(b,b.base),member={...job(b,mc),id:"rel_m",entryId:"tqe_m",state:"verified"},la=fake(),lc=config(b,lead);lc.batch={max:3,candidates:[member]};
 la.batch=async op=>{la.calls.push(op);return true;};la.merged=async commit=>la.calls.push("merged:"+commit);
 assert.equal((await runRelease(lc,la)).commit,b.base);assert.deepEqual(la.calls,["merged:"+b.base,"finish"]);
 const lj=JSON.parse(readFileSync(lc.journalPath,"utf8"));assert.equal(lj.noChange,NO_CHANGE);assert.ok(!lj.batch);assert.deepEqual(checkoutState(b.cwd),{status:"",head:btip,ref:btip});
});
test("n1 a retry after a rolled-back fast-forward release is cherry-picked again, not settled as no change",async()=>{
 // The release fast-forwards to c; the rollback's revert r keeps c an ancestor
 // of tasks-hub without its content.
 const rolledBack=()=>{
  const f=fixture(),c=change(f,"client/a.js","a"),j=job(f,c),first=integrateCandidate(f.cwd,j);assert.equal(first.integrated,c);
  publishIntegration(f.cwd,c,f.base);const r=revertCommit(f.cwd,j,c,f.base);assert.equal(moveReleaseRef(f.cwd,r,c),true);git(f.cwd,"checkout","--detach",r);
  assert.ok(!existsSync(join(f.cwd,"client/a.js")));return {f,c,j,r};
 };
 const s=rolledBack(),retry=integrateCandidate(s.f.cwd,s.j);
 assert.equal(retry.noChange,undefined);assert.equal(retry.expected,s.r);assert.deepEqual(retry.changed,["client/a.js"]);
 assert.equal(git(s.f.cwd,"rev-parse","HEAD^"),s.r);assert.equal(readFileSync(join(s.f.cwd,"client/a.js"),"utf8"),"a");
 // As a batch member behind another lead it is picked and joins the batch.
 const b=rolledBack();git(b.f.cwd,"checkout","-b","lead",b.f.base);const lc=change(b.f,"client/l.js","l");
 const lead={...job(b.f,lc),id:"rel_lead",entryId:"tqe_lead"},member={...b.j,id:"rel_retry",entryId:"tqe_retry"};
 const picked=await integrateBatch(b.f.cwd,lead,[member],3,{open:async()=>"bat_0000000000000000",add:async()=>true});
 assert.deepEqual(picked.dropped,[]);assert.deepEqual(picked.batch.jobs.map(j=>j.jobId),["rel_lead","rel_retry"]);
 assert.equal(picked.expected,b.r);assert.deepEqual(picked.changed,["client/a.js","client/l.js"]);
 // A path the tip holds with other content is not carried either: the pick
 // runs and meets the tip's later change.
 const o=fixture(),oc=change(o,"client/a.js","a");git(o.cwd,"checkout","tasks-hub");git(o.cwd,"merge","--ff-only","candidate");change(o,"client/a.js","later");
 refusesUnchanged(o.cwd,job(o,oc),"integration-conflict: client/a.js");
});
test("n1 a no-change job whose merged call fails ends blocked for reconciliation and is never refused",async()=>{
 const f=fixture(),tip=advanced(f),j=job(f,f.base),a=fake(),c=config(f,j);
 a.merged=async()=>{a.calls.push("merged");throw releaseError("Native release command failed");};a.escalate=async d=>a.calls.push("escalate:"+d.outcome);
 await assert.rejects(runRelease(c,a),/Release failed; inspect saved journal/);
 assert.deepEqual(a.calls,["merged","escalate:blocked","block"]);
 const journal=JSON.parse(readFileSync(c.journalPath,"utf8"));
 assert.equal(journal.phase,"blocked");assert.equal(journal.outcome,"blocked");assert.equal(journal.mergedSent,true);assert.equal(journal.noChange,NO_CHANGE);
 assert.notEqual(journal.published,true);assert.equal(journal.revert,undefined);assert.equal(journal.refusalReason,undefined);assert.deepEqual(journal.effects,[]);
 assert.deepEqual(checkoutState(f.cwd),{status:"",head:tip,ref:tip});
 // The next claim does not run it again: the journal waits for the handler.
 const again=fake();await assert.rejects(runRelease(c,again),e=>failureReason(e)==="Ambiguous journal requires handler reconciliation");assert.deepEqual(again.calls,[]);
});
test("n2 an empty series is a no-op and a merge commit is refused, alone and as batch members",async()=>{
 // A commit equal to its base that is not on tasks-hub still changes nothing.
 const x=fixture();git(x.cwd,"checkout","-b","side",x.base);const side=change(x,"client/s.js","s"),xtip=advanced(x);
 assert.deepEqual(integrateCandidate(x.cwd,{...job(x,side),baseCommit:side}),{expected:xtip,integrated:xtip,changed:[],noChange:NO_CHANGE});
 // A merge commit in the series is still nonlinear.
 const n=fixture();change(n,"client/a.js","a");git(n.cwd,"checkout","-b","merged-side",n.base);change(n,"client/s.js","s");git(n.cwd,"checkout","candidate");git(n.cwd,"merge","--no-ff","-m","merge","merged-side");
 const merged=git(n.cwd,"rev-parse","HEAD");advanced(n);refusesUnchanged(n.cwd,job(n,merged),"Nonlinear candidate series");
 // As batch members behind a lead with a real change: the member equal to the
 // shared base joins nothing (the hub refuses a range that adds no commit),
 // and each refusal keeps its own reason.
 const f=fixture(),lc=change(f,"client/a.js","a");git(f.cwd,"checkout","-b","side",f.base);const fside=change(f,"client/s.js","s");
 git(f.cwd,"checkout","-b","merged-side",f.base);change(f,"client/t.js","t");git(f.cwd,"checkout","side");git(f.cwd,"merge","--no-ff","-m","merge","merged-side");
 const fmerge=git(f.cwd,"rev-parse","HEAD"),tip=advanced(f),calls=[];
 const lead=job(f,lc),members=[{...job(f,f.base),id:"rel_same"},{...job(f,fside),baseCommit:fside,id:"rel_empty"},{...job(f,fmerge),id:"rel_merge"}];
 const r=await integrateBatch(f.cwd,lead,members,8,{open:async()=>{calls.push("open");return "bat_0000000000000000";},add:async()=>{calls.push("add");return true;}});
 assert.deepEqual(r.dropped,[{jobId:"rel_same",reason:"batch-no-change"},{jobId:"rel_empty",reason:"batch-no-change"},{jobId:"rel_merge",reason:"Nonlinear candidate series"}]);
 assert.deepEqual(calls,[]);assert.equal(r.batch,undefined);assert.equal(r.expected,tip);assert.deepEqual(r.changed,["client/a.js"]);
 assert.equal(git(f.cwd,"rev-parse","HEAD"),r.integrated);assert.equal(git(f.cwd,"rev-parse","HEAD^"),tip);assert.equal(git(f.cwd,"status","--porcelain"),"");
 // A lead with no change stops before any member, with no batch call.
 const g=fixture(),gtip=advanced(g);git(g.cwd,"checkout","candidate");const gc=change(g,"client/m.js","m");
 const none=await integrateBatch(g.cwd,job(g,g.base),[{...job(g,gc),id:"rel_m"}],8,{open:async()=>{calls.push("open");return "bat_0000000000000000";},add:async()=>{calls.push("add");return true;}});
 assert.deepEqual(none,{expected:gtip,integrated:gtip,changed:[],noChange:NO_CHANGE,dropped:[]});assert.deepEqual(calls,[]);assert.deepEqual(checkoutState(g.cwd),{status:"",head:gtip,ref:gtip});
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
 const pinnedHost=new HostAdapter({cwd:fakeWrangler(),journalDirectory:home},{id:"rel_tags"});pinnedHost.command=()=>"no url";await assert.rejects(pinnedHost.deploy("tailos",{commit}),tagged("TailOS deployment identity unconfirmed"));
 // Through the runner: the real adapter's prepare has no input for the target.
 const g=fixture(),j=job(g,change(g,"client/a.js","a")),r=await journalOf(g,j,(a,c)=>{
  const host=new HostAdapter({cwd:g.cwd,journalDirectory:dirname(c.journalPath),baselines:c.baselines,targets:{tailos:{}}},{...j});importInputs(host,j.commit,{});a.prepare=(t,commit)=>host.prepare(t,commit);
 },/Release failed/);
 assert.deepEqual(r.journal.failure,{step:"prepare",target:"tailos",reason:"Exact job target input required"});assert.deepEqual(r.journal.failureDetail,{text:"Exact job target input required"});
 assert.deepEqual(Object.keys(r.journal.receipt).sort(),["commit","failure","jobId","outcome","revert","targets","verificationDigest","version"]);assert.deepEqual(r.journal.receipt.failure,r.journal.failure);
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
// w1-w3: a fake pinned wrangler install. No test runs wrangler or npx: every
// host command goes through a capturing adapter.command.
const WRANGLER="4.145.0",WRANGLER_TAG="Pinned wrangler not installed; run npm ci";
function fakeWrangler(cwd=mkdtempSync(join(tmpdir(),"release-wrangler-")),{pin=WRANGLER,installed=WRANGLER,bin=true,manifest=true}={}){
 if(manifest)writeFileSync(join(cwd,"package.json"),JSON.stringify({devDependencies:{wrangler:pin}}));
 if(installed){mkdirSync(join(cwd,"node_modules/wrangler"),{recursive:true});writeFileSync(join(cwd,"node_modules/wrangler/package.json"),JSON.stringify({version:installed}));}
 if(bin){mkdirSync(join(cwd,"node_modules/.bin"),{recursive:true});writeFileSync(join(cwd,"node_modules/.bin/wrangler"),"");}
 return cwd;
}
const tailosRollback=(bin,commit)=>[bin,"pages","deploy","/journal/tailos-dist-"+commit,"--project-name","tailos","--branch","main","--commit-hash",commit,"--commit-dirty=false"];
test("wrangler w1 package.json and the lockfile pin one exact wrangler version",()=>{
 const pkg=JSON.parse(readFileSync(new URL("../package.json",import.meta.url),"utf8")),lock=JSON.parse(readFileSync(new URL("../package-lock.json",import.meta.url),"utf8"));
 const pin=pkg.devDependencies.wrangler;assert.match(pin,/^\d+\.\d+\.\d+$/);assert.equal(pin,WRANGLER);
 assert.equal(lock.packages[""].devDependencies.wrangler,pin);assert.equal(lock.packages["node_modules/wrangler"].version,pin);
});
test("wrangler w1 the TailOS deploy runs the pinned checkout binary",async()=>{
 const cwd=fakeWrangler(),commit="c".repeat(40),calls=[],adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});
 adapter.command=argv=>{calls.push(argv);return "https://abcdef12.tailos.pages.dev";};
 const a={commit};await adapter.deploy("tailos",a);
 assert.deepEqual(calls,[[join(cwd,"node_modules/.bin/wrangler"),"pages","deploy","dist-static","--project-name","tailos","--branch","main","--commit-hash",commit,"--commit-dirty=false"]]);
 assert.equal(a.deployment,"https://abcdef12.tailos.pages.dev");
});
test("wrangler w1 no release or old-site script calls npx",()=>{
 for(const name of ["release-runner.mjs","release-inputs.mjs","deploy-static.mjs"]){
  const source=readFileSync(new URL("../scripts/"+name,import.meta.url),"utf8");assert.ok(!source.includes('"npx"'),name);assert.ok(!/["']wrangler["']\s*,\s*["']pages["']/.test(source),name);
 }
 assert.ok(readFileSync(new URL("../scripts/deploy-static.mjs",import.meta.url),"utf8").includes('"node_modules/.bin/wrangler"'));
});
test("wrangler w2 a missing pinned wrangler refuses the job before publication and before any target deploys",async()=>{
 const g=fixture();mkdirSync(join(g.cwd,"hub/cmd/tt"),{recursive:true});change(g,"hub/cmd/tt/x.go","x");const j=job(g,change(g,"client/a.js","a"));
 const before=git(g.cwd,"rev-parse","refs/heads/tasks-hub");let tools=0;
 const r=await journalOf(g,j,(a,c)=>{
  const host=new HostAdapter({cwd:g.cwd,journalDirectory:dirname(c.journalPath),baselines:c.baselines,targets:{tailos:{}}},{...j});
  a.releaseTools=commit=>{tools++;return host.releaseTools(commit);};
 });
 assert.equal(tools,1);
 assert.deepEqual(r.journal.failure,{step:"prepare",target:"tailos",reason:WRANGLER_TAG});assert.equal(r.journal.refusalReason,WRANGLER_TAG);
 assert.notEqual(r.journal.published,true);assert.deepEqual(r.journal.effects,[]);
 assert.ok(r.a.calls.includes("refuse"));assert.ok(!r.a.calls.includes("merged"));assert.ok(!r.a.calls.some(c=>String(c).startsWith("deploy:")));
 assert.equal(git(g.cwd,"rev-parse","refs/heads/tasks-hub"),before);
});
test("wrangler w2 a claim lost while releaseTools runs is refused unpublished: the fence check stays directly before publication",async()=>{
 const g=fixture(),j=job(g,change(g,"client/a.js","a")),before=git(g.cwd,"rev-parse","refs/heads/tasks-hub");let lost=false,after=0;
 const r=await journalOf(g,j,a=>{
  a.releaseTools=async()=>{a.calls.push("releaseTools");lost=true;};
  a.fence=async()=>{if(lost)after++;return !lost;};
 });
 assert.ok(after>=1,"a fence check after releaseTools");assert.equal(r.journal.refusalReason,"release fence lost");
 assert.notEqual(r.journal.published,true);assert.deepEqual(r.journal.effects,[]);
 assert.ok(r.a.calls.includes("releaseTools"));assert.ok(r.a.calls.includes("refuse"));assert.ok(!r.a.calls.includes("merged"));assert.ok(!r.a.calls.some(c=>String(c).startsWith("deploy:")));
 assert.equal(git(g.cwd,"rev-parse","refs/heads/tasks-hub"),before);
});
test("wrangler w2 releaseTools refuses a missing binary, a different installed version and a non-exact pin",async()=>{
 const g=fixture(),commit=change(g,"client/a.js","a");git(g.cwd,"checkout","--detach",commit);
 const tagged=e=>failureReason(e)===WRANGLER_TAG;
 const tools=(over)=>{const cwd=over===null?g.cwd:fakeWrangler(g.cwd,over);const h=new HostAdapter({cwd,journalDirectory:cwd,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,g.base]))},{id:"rel_fixture"});const cmds=[];h.command=argv=>{cmds.push(argv);return "";};return {h,cmds};};
 for(const over of [null,{installed:"4.144.0"},{pin:"^"+WRANGLER},{bin:false}]){
  rmSync(join(g.cwd,"node_modules"),{recursive:true,force:true});const {h,cmds}=tools(over);
  await assert.rejects(h.releaseTools(commit),tagged,JSON.stringify(over));assert.deepEqual(cmds,[],"no lockfile change, no npm ci");
 }
 rmSync(join(g.cwd,"node_modules"),{recursive:true,force:true});const ok=tools({});await ok.h.releaseTools(commit);assert.deepEqual(ok.cmds,[]);
 // The deploy checks too, and runs nothing when the install is missing.
 const empty=mkdtempSync(join(tmpdir(),"release-wrangler-none-")),d=new HostAdapter({cwd:empty,journalDirectory:empty},{id:"rel_fixture"}),ran=[];d.command=argv=>{ran.push(argv);return "https://abcdef12.tailos.pages.dev";};
 await assert.rejects(d.deploy("tailos",{commit}),tagged);assert.deepEqual(ran,[]);
});
test("wrangler w2 releaseTools runs npm ci once when the lockfile changed and prepare does not repeat it",async()=>{
 const g=fixture();change(g,"package.json",JSON.stringify({devDependencies:{wrangler:WRANGLER}}));const commit=change(g,"package-lock.json","{}");git(g.cwd,"checkout","--detach",commit);
 const home=mkdtempSync(join(tmpdir(),"release-wrangler-ci-")),cmds=[];
 const h=new HostAdapter({cwd:g.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(t=>[t,g.base])),targets:{tailos:{}}},{...job(g,commit),id:"rel_fixture"});
 h.command=argv=>{cmds.push(argv.join(" "));if(argv.join(" ")==="npm ci")fakeWrangler(g.cwd,{manifest:false});return "";};
 await h.releaseTools(commit);assert.deepEqual(cmds,["npm ci"]);
 importInputs(h,commit,{tailos:{release:"rel_fixture-"+commit.slice(0,12)+"-tailos"}});
 await assert.rejects(h.prepare("tailos",commit),e=>failureReason(e)!==WRANGLER_TAG);
 assert.deepEqual(cmds,["npm ci","npm run build:static","npm run verify:release"]);
});
test("wrangler w2 a pinned TailOS rollback with no install throws before running, and an old npx program runs as recorded",async()=>{
 const commit="5".repeat(40),probe=[process.execPath,"probe"];
 const rollback=(cwd,program)=>{const h=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"}),ran=[];h.command=argv=>{ran.push(argv);return JSON.stringify({restored:true,databaseWritesPreserved:true});};h.artifacts.set("tailos",{rollbackSafe:true,rollbackProgram:program,rollbackProbe:probe});return {h,ran};};
 const empty=mkdtempSync(join(tmpdir(),"release-wrangler-rollback-")),missing=rollback(empty,tailosRollback(wranglerBin(empty),commit));
 await assert.rejects(missing.h.rollback("tailos"),e=>failureReason(e)===WRANGLER_TAG);assert.deepEqual(missing.ran,[]);
 const cwd=fakeWrangler(),pinned=rollback(cwd,tailosRollback(wranglerBin(cwd),commit));
 assert.equal(await pinned.h.rollback("tailos"),true);assert.deepEqual(pinned.ran,[tailosRollback(join(cwd,"node_modules/.bin/wrangler"),commit),probe]);
 const old=["npx","wrangler",...tailosRollback("x",commit).slice(1)],recorded=rollback(empty,old);
 assert.equal(await recorded.h.rollback("tailos"),true);assert.deepEqual(recorded.ran,[old,probe]);
});
test("g1 the runner throws no untagged fixed error",()=>{
 const source=readFileSync(RUNNER,"utf8");assert.ok(!source.includes("throw new Error("));assert.equal(source.split("new Error(").length-1,2,"releaseError and the command wrapper");
 for(const tag of ["Release binding mismatch","Dirty deployment checkout","Unknown commit identity","Unknown candidate base","Nonlinear candidate series","Candidate integration refused","Integrated checkout changed","Release ref race","Ambiguous journal requires handler reconciliation","Release host locked; inspect prior execution","Fresh job backup identity required","Exact job target input required","Candidate build checkout mismatch","TailOS deployment identity unconfirmed","Pinned wrangler not installed; run npm ci"]){
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
  const commands=readFileSync(f.log,"utf8").trim().split("\n");assert.ok(commands.every(x=>x.startsWith("deployment list ") || x.startsWith("deployment progress ")));
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
 assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);assert(!existsSync(join(dir,jobID+"-inputs.json")));assert(!existsSync(dir) || !readdirSync(dir).some(n=>n.endsWith("-inputs.json")),"no manifest of any generation");
});

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
 const cases=[[build({state:"draining"}),"draining"],[build({state:"restart"}),"restart"],...CODE_REASONS.map(reason=>[build({state:"refused",reason}),"refused"]),[build({state:"refused",reason:"load-failed",checkout:"restored",loadedCommit:r.a}),"refused"],
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
 const hostileHead=codeNotice({state:"refused",reason:"load-failed",checkout:"restored",loadedCommit:"../../etc/passwd",runId:CODE_RUN,loaded:{digest:r.codeA.digest},current:{commit:r.b,digest:r.codeB.digest},changed:[]});
 assert.match(hostileHead.text,SAFE_TEXT);assert.ok(!/passwd/.test(hostileHead.text));assert.ok(hostileHead.text.includes("loaded commit none."));
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
 assert.deepEqual(entry.argv,[h.tt,"deployment","refuse","--job","rel_cli","--generation","2","--request-id","rel_cli-refuse-2","--reason","Release binding mismatch","--step","integrate"]);
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
test("held r1 r2 r3 a refused claim sends exactly one notice with the hub's reason in a deployer run, the job stays skipped and nothing private leaves",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-held-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 // The hub refuses rel_held only; its stderr ends with the refusal, a token, a NAME=value pair and the journal path.
 const h=codeHost(t,cwd,`if(a[1]==='claim'&&flag('--job')==='rel_held'){fs.writeSync(2,'debug line with '+${JSON.stringify(HELD_SECRET)}+'\\ntt: hub: 409 conflict: release: candidate changed since enqueue token '+${JSON.stringify(HELD_SECRET)}+' TAILTERM_TOKEN='+${JSON.stringify(HELD_SECRET)}+' '+__dirname+'/rel_held.json\\n');process.exit(1);}`+heldRoster);
 const held={...verifiedJob("rel_held"),itemId:HELD_ITEM},next=verifiedJob("rel_next"),released=[];h.setJobs([held,next]);
 const err=await heldRun(t,h,3,async c=>{released.push(c.job.id);});
 // Skip unchanged: every poll tries the held job, prints the line, and goes on to the later job.
 const claims=h.claims().map(c=>c.argv[c.argv.indexOf("--job")+1]);
 assert.deepEqual(claims,["rel_held","rel_next","rel_held","rel_next","rel_held","rel_next"]);assert.deepEqual(released,["rel_next","rel_next","rel_next"]);
 assert.equal(err,"Release claim held; handler reconciliation required.\n".repeat(3));
 // Over three polls exactly one typed notice to each of the handler and the owner helper, and none to the Board.
 const sent=h.calls().filter(c=>c.argv[0]==="send").map(sendOf);assert.deepEqual(sent.map(s=>s.to),[HELD_HANDLER,HELD_HELPER]);const [n,copy]=sent;
 const id=heldClaimNotice(held,"claim","hub: 409 conflict: release: candidate changed since enqueue token (removed) (tt deployment claim exit 1)",CODE_RUN).requestId;
 assert.match(id,new RegExp("^rel_held-claim-held-"+CODE_RUN+"-[a-f0-9]{8}$"));assert.equal(n.requestId,id+"-handler");assert.equal(copy.requestId,id+"-helper");
 assert.deepEqual({...copy,to:null,requestId:null},{...n,to:null,requestId:null});
 assert.equal(n.kind,"notice");assert.equal(n.subject,"A release job is skipped because the hub refused its claim");
 // The roster and the handler are read at the first poll only.
 assert.equal(h.calls().filter(c=>c.argv[0]==="agents").length,1);assert.equal(h.calls().filter(c=>c.argv[1]==="handler").length,1);
 assert.deepEqual(n.refs,["release-job=rel_held","item="+HELD_ITEM,"commit="+"c".repeat(40)]);
 assert.ok(n.text.includes("Release rel_held (item "+HELD_ITEM+", commit "+"c".repeat(12)+")"));
 assert.ok(n.text.includes("Reason: hub: 409 conflict: release: candidate changed since enqueue token (removed) (tt deployment claim exit 1)."));
 assert.match(n.text,/Database handler: reconcile the job .*Owner helper: for information\.$/);assert.match(n.text,SAFE_TEXT);assert.ok(n.subject.length<=120&&!/rel_|wi_/.test(n.subject));
 // Nothing private in any argv word of any call: not the secret, the variable or the journal directory.
 for(const c of h.calls())for(const word of c.argv)for(const secret of [HELD_SECRET,"SYNTHETIC","TAILTERM_TOKEN",h.home,tmpdir()])assert.ok(!word.includes(secret),secret);
 // The exact stderr stays in the private record only.
 assert.ok(JSON.parse(readFileSync(join(h.home,"cli-failures.json"),"utf8")).failures[0].stderr.includes(HELD_SECRET));
});
test("held r1 r2 a held set-aside journal is never claimed; a recipient that cannot be resolved or reached gets a Board notice and is retried",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-held-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 // At the first poll the handler lookup fails and the post to the owner helper fails; from the second both work.
 const once=(name,code)=>`if(!fs.existsSync(__dirname+'/${name}')){fs.writeFileSync(__dirname+'/${name}','');process.exit(${code});}`;
 const h=codeHost(t,cwd,`if(a[0]==='deployment'&&a[1]==='handler'){${once("handler-failed",2)}}if(a[0]==='send'&&a.includes(${JSON.stringify(HELD_HELPER)})){${once("helper-failed",4)}}`+heldRoster);
 const held={...verifiedJob("rel_aside"),itemId:HELD_ITEM,reconciliations:[{disposition:"set_aside",agentId:"agt_old",runId:"run_old"}]},released=[];
 // A journal of another run: not exactly the set-aside claim.
 writeFileSync(join(h.home,"rel_aside.json"),JSON.stringify({jobId:"rel_aside",commit:held.commit,agentId:"agt_other",runId:"run_other",effects:[]}));
 h.setJobs([held,verifiedJob("rel_next")]);
 const err=await heldRun(t,h,4,async c=>{released.push(c.job.id);});
 assert.deepEqual(h.claims().map(c=>c.argv[c.argv.indexOf("--job")+1]),Array(4).fill("rel_next"));assert.deepEqual(released,Array(4).fill("rel_next"));
 assert.ok(existsSync(join(h.home,"rel_aside.json")),"the held journal is left in place");
 assert.equal(err,"Set-aside release journal held; handler reconciliation required.\nHeld claim notice not delivered to every recipient; the next poll retries.\n"+"Set-aside release journal held; handler reconciliation required.\n".repeat(3));
 // Poll one: the failed post to the helper, then the Board notice. Poll two: one to each. Polls three and four: nothing.
 const sent=h.calls().filter(c=>c.argv[0]==="send").map(sendOf),id=heldClaimNotice(held,"journal","the journal is unreadable, shows effects or is not of exactly the set-aside claim",CODE_RUN).requestId;
 assert.deepEqual(sent.map(s=>[s.to,s.requestId]),[[HELD_HELPER,id+"-helper"],[null,id+"-board"],[HELD_HANDLER,id+"-handler"],[HELD_HELPER,id+"-helper"]]);
 for(const n of sent){
  assert.equal(n.kind,"notice");assert.equal(n.subject,"A release job is skipped because its set-aside journal is held");
  assert.deepEqual(n.refs,["release-job=rel_aside","item="+HELD_ITEM,"commit="+"c".repeat(40),...(n.to?[]:["wake=attention"])]);
  assert.ok(n.text.includes("Reason: the journal is unreadable, shows effects or is not of exactly the set-aside claim."));assert.match(n.text,SAFE_TEXT);
 }
 for(const word of h.calls().flatMap(c=>c.argv))assert.ok(!word.includes(h.home));
 // A new deployer process sends each recipient its notice once more, under the same request ids.
 await h.poll(null,async()=>{});assert.deepEqual(h.calls().filter(c=>c.argv[0]==="send").map(sendOf).slice(4).map(s=>[s.to,s.requestId]),[[HELD_HANDLER,id+"-handler"],[HELD_HELPER,id+"-helper"]]);
});
test("held r1 r2 with no live owner helper and no handler the Board notice is sent once per deployer run",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-held-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 // Two live helpers are as unusable as none; the handler lookup fails.
 const h=codeHost(t,cwd,`if(a[1]==='claim'&&flag('--job')==='rel_held'){fs.writeSync(2,'tt: hub: 409 conflict: release: job cannot be taken over\\n');process.exit(1);}
if(a[0]==='agents'){console.log(JSON.stringify([{id:'agt_aa',role:'owner_helper',status:'running'},{id:'agt_bb',role:'owner_helper',status:'retired'}]));process.exit(0);}`);
 h.setJobs([{...verifiedJob("rel_held"),itemId:HELD_ITEM},verifiedJob("rel_next")]);
 const err=await heldRun(t,h,3,async()=>{});
 // The undelivered line is printed once per job and process (held c3).
 assert.equal(err,"Release claim held; handler reconciliation required.\nHeld claim notice not delivered to every recipient; the next poll retries.\n"+"Release claim held; handler reconciliation required.\n".repeat(2));
 const sent=h.calls().filter(c=>c.argv[0]==="send").map(sendOf);assert.equal(sent.length,1);assert.equal(sent[0].to,null);assert.match(sent[0].requestId,/-board$/);
 assert.ok(sent[0].text.includes("Reason: hub: 409 conflict: release: job cannot be taken over (tt deployment claim exit 1)."));
 assert.equal(h.claims().filter(c=>c.argv.includes("rel_next")).length,3);
});
test("held c3 while no owner helper can be resolved the line prints once per job and the roster is read at most every five minutes",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-held-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 // Two held claims; the handler resolves, the owner helper appears only once the file "helper" exists.
 const h=codeHost(t,cwd,`if(a[1]==='claim'&&flag('--job').startsWith('rel_held')){fs.writeSync(2,'tt: hub: 409 conflict: release: job cannot be taken over\\n');process.exit(1);}
if(a[0]==='deployment'&&a[1]==='handler'){console.log(JSON.stringify({id:${JSON.stringify(HELD_HANDLER)}}));process.exit(0);}
if(a[0]==='agents'){console.log(JSON.stringify(fs.existsSync(__dirname+'/helper')?[{id:${JSON.stringify(HELD_HELPER)},role:'owner_helper',status:'running'}]:[]));process.exit(0);}`);
 h.setJobs([{...verifiedJob("rel_held1"),itemId:HELD_ITEM},{...verifiedJob("rel_held2"),itemId:HELD_ITEM},verifiedJob("rel_next")]);
 const saved=heldRetry.now;t.after(()=>{heldRetry.now=saved;});
 // Poll one at 0; poll two just inside five minutes; poll three at five minutes, with the helper live; poll four after it.
 const clock=[0,299999,300000,300001];let poll=0;heldRetry.now=()=>clock[poll];
 const rosterReads=[],readsNow=()=>h.calls().filter(c=>c.argv[0]==="agents").length;
 const err=await heldRun(t,h,4,async()=>{rosterReads.push(readsNow());poll++;if(poll===2)writeFileSync(join(h.home,"helper"),"");});
 // One read at poll one, none at poll two, then one per job when the helper resolves at poll three, and none after delivery.
 assert.deepEqual(rosterReads,[1,1,3,3]);
 const held="Release claim held; handler reconciliation required.\n",line="Held claim notice not delivered to every recipient; the next poll retries.\n";
 assert.equal(err,(held+line).repeat(2)+held.repeat(6));
 // Each job: its handler notice and Board copy at poll one, its helper notice once at poll three.
 const sent=h.calls().filter(c=>c.argv[0]==="send").map(sendOf);
 const idOf=id=>heldClaimNotice({...verifiedJob(id),itemId:HELD_ITEM},"claim","hub: 409 conflict: release: job cannot be taken over (tt deployment claim exit 1)",CODE_RUN).requestId;
 assert.deepEqual(sent.map(s=>[s.to,s.requestId]),[[HELD_HANDLER,idOf("rel_held1")+"-handler"],[null,idOf("rel_held1")+"-board"],[HELD_HANDLER,idOf("rel_held2")+"-handler"],[null,idOf("rel_held2")+"-board"],[HELD_HELPER,idOf("rel_held1")+"-helper"],[HELD_HELPER,idOf("rel_held2")+"-helper"]]);
 assert.deepEqual(sent.map(s=>s.refs.includes("wake=attention")),[false,true,false,true,false,false]);
 assert.equal(h.claims().filter(c=>c.argv.includes("rel_next")).length,4);
});
test("held r3 the notice keeps only plain words, small numbers and hub ids of the refusal, and only well-formed job fields",()=>{
 assert.equal(refusalText("noise\ntt: hub: 409 conflict: release: job rel_0123abcd is claimed; reconcile it first\n"),"hub: 409 conflict: release: job rel_0123abcd is claimed; reconcile it first");
 assert.equal(refusalText(Buffer.from("tt: open /Users/someone/.tailterm/journal/rel_x.json: permission denied")),"open (removed) permission denied");
 assert.equal(refusalText("refused Bearer abc123DEF456 TOKEN=hunter2 ~/.config/tt ../x C:\\\\secret 1234567 $HOME `id`"),"refused Bearer (removed)");
 for(const empty of [undefined,null,"","\n\n","/only/a/path","abc123 /p"])assert.equal(refusalText(empty),null);
 const long=refusalText("word ".repeat(300));assert.ok(long.length<=120&&long.startsWith("word word"));
 const n=heldClaimNotice({id:"rel_a",itemId:"wi_../../etc",commit:"not a commit"},"claim","bad\nreason "+HELD_SECRET+"!","run with spaces");
 assert.ok(n.text.includes("(item unknown, commit unknown)"));assert.ok(n.text.includes("Reason: unclassified."));assert.equal(n.item,null);assert.equal(n.commit,null);assert.match(n.requestId,/^rel_a-claim-held-unknown-[a-f0-9]{8}$/);assert.match(n.text,SAFE_TEXT);
 // The request id follows the case and the reason, nothing else.
 const idOf=(kind,reason)=>heldClaimNotice({id:"rel_a"},kind,reason,"run_1").requestId;
 assert.equal(idOf("claim","one reason"),idOf("claim","one reason"));assert.notEqual(idOf("claim","one reason"),idOf("claim","another reason"));assert.notEqual(idOf("claim","one reason"),idOf("journal","one reason"));
 assert.equal(heldClaimNotice({id:"../rel"},"claim","x","r"),null);assert.equal(heldClaimNotice(null,"claim","x","r"),null);
});
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
// A failed job-list read (wi_56ac00ba2854fa3d c1).
const DRAIN_LINE="Deployer code is out of date; no new release is claimed and a restart follows the active work.\n",RESTART_LINE="Deployer restarting itself onto the published scripts.\n",REFUSED_LINE="Deployer code is out of date and it cannot restart itself; no release is claimed until it is re-provisioned.\n";
test("code c1 a poll whose job list cannot be read still records its code and drains while its own journal shows an unfinished job",async t=>{
 // [name, arrange(h,r) returning an undo or nothing]
 const cases=[
  ...["waiting_inputs","blocked","refusing",undefined].map(phase=>[`this run's journal is ${phase}`,h=>{codeJournal(h,"rel_own",{phase});}]),
  ["a journal is unreadable",h=>{writeFileSync(join(h.home,"rel_bad.json"),"{not json");}],
  ["this process has a live matrix child",()=>{const children=new HostAdapter({},{}).matrixChildren,key="code-c1-fixture";children.set(key,{});return ()=>children.delete(key);}],
  ["a host release lock exists at a journal's task lock path",(h,r)=>{codeJournal(h,"rel_other",{agentId:"agt_other",runId:"run_other",phase:"waiting_inputs",taskId:"tsk_c1"});const lock=taskLock(r.cwd,"tsk_c1");mkdirSync(dirname(lock),{recursive:true});writeFileSync(lock,"{}");return ()=>rmSync(lock,{force:true});}],
 ];
 for(const [name,arrange] of cases){
  const r=codeRepo(t),h=codeHost(t,r.cwd,LIST_FAILS),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);h.setJobs([verifiedJob()]);failList(h);
  const undo=arrange(h,r)||(()=>{});t.after(undo);let err;
  try{err=await h.poll(gate)+await h.poll(gate);}finally{undo();}
  assert.equal(execs.length,0,name);assert.equal(r.head(),r.a,name);assert.equal(h.claims().length,0,name);
  assert.equal(h.record().state,"draining",name);assert.equal(h.record().current.digest,r.codeB.digest,name);
  assert.deepEqual(codeSends(h).map(s=>s.requestId),[`runner-code-${CODE_RUN}-${r.codeA.digest.slice(0,12)}-${r.codeB.digest.slice(0,12)}-draining`],name);
  // The failed list call keeps its own notice, as before.
  assert.ok(h.sends().filter(s=>!s.requestId.startsWith("runner-code-")).every(s=>s.requestId.startsWith(`cli-failure-${CODE_RUN}-`)),name);
  assert.ok(err.startsWith(DRAIN_LINE+"Deployment poll held ("),name);assert.equal(err.split("Deployment poll held (").length-1,2,name);
 }
 // The last ledger's lock path counts too: read once with a blocked job of a task, then the list fails while that lock exists.
 const r=codeRepo(t),h=codeHost(t,r.cwd,LIST_FAILS),{gate,execs}=codeGate(r.codeA),lock=taskLock(r.cwd,"tsk_last");r.at(r.a,r.b);
 h.setJobs([{id:"rel_blocked",state:"blocked",generation:3,commit:"d".repeat(40),taskId:"tsk_last"}]);t.after(()=>rmSync(lock,{force:true}));
 const err=await codeServe(t,h,gate,[()=>{failList(h);mkdirSync(dirname(lock),{recursive:true});writeFileSync(lock,"{}");}],(n,err)=>n<2||err.includes("Deployment poll held ("));rmSync(lock,{force:true});
 assert.equal(execs.length,0);assert.equal(r.head(),r.a);assert.equal(h.record().state,"draining");assert.equal(codeSends(h).length,1);assert.equal(err.split("Deployment poll held (").length-1,1);
 // Without that lock the same second poll restarts.
 const s=codeRepo(t),g=codeHost(t,s.cwd,LIST_FAILS),free=codeGate(s.codeA);s.at(s.a,s.b);
 g.setJobs([{id:"rel_blocked",state:"blocked",generation:3,commit:"d".repeat(40),taskId:"tsk_last"}]);
 await codeServe(t,g,free.gate,[()=>{failList(g);}],n=>n<2||free.execs.length===1);assert.equal(free.execs.length,1);assert.equal(s.head(),s.b);
});
test("code c1 an idle runner whose job list cannot be read restarts once onto the published scripts",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd,LIST_FAILS),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);h.setJobs([verifiedJob()]);failList(h);
 // Only a finished journal of this run, unfinished journals of other runs, and files that are not journals.
 codeJournal(h,"rel_done",{phase:"complete"});codeJournal(h,"rel_refused",{phase:"refused"});codeJournal(h,"rel_other",{agentId:"agt_other",runId:"run_other",phase:"waiting_matrix"});codeJournal(h,"rel_agent",{agentId:"agt_other",phase:"refusing"});
 writeFileSync(join(h.home,"rel_done-g2-inputs.json"),"{not json");writeFileSync(join(h.home,"rel_old.json.set-aside-g1"),"{not json");writeFileSync(join(h.home,"rel_old.json.unclaimed-g2"),"{not json");
 const seen=[];gate.execve=(...args)=>{execs.push(args);seen.push({record:h.record(),head:r.head()});};
 const err=await h.poll(gate);
 assert.equal(execs.length,1);assert.equal(r.head(),r.b);assert.equal(h.claims().length,0);assert.equal(seen[0].record.state,"restart");assert.equal(seen[0].head,r.b);
 assert.equal(h.record().state,"restart");assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer is restarting itself onto the published scripts"]);
 assert.equal(execs[0][2].TAILTERM_RUNNER_REEXEC,r.codeB.digest);assert.ok(err.startsWith(RESTART_LINE+"Deployment poll held ("),err);
 // The predicate alone.
 const base={journalDirectory:h.home,cwd:r.cwd,agent:CODE_AGENT,run:CODE_RUN,lastLock:null,matrixChildren:0};
 assert.equal(journalIdle(base),true);
 for(const over of [{matrixChildren:1},{matrixChildren:undefined},{agent:undefined},{run:""},{journalDirectory:join(h.home,"absent")}])assert.equal(journalIdle({...base,...over}),false,JSON.stringify(over));
 const lock=taskLock(r.cwd,"tsk_any");mkdirSync(dirname(lock),{recursive:true});writeFileSync(lock,"{}");t.after(()=>rmSync(lock,{force:true}));
 assert.equal(journalIdle({...base,lastLock:lock}),false);rmSync(lock);assert.equal(journalIdle({...base,lastLock:lock}),true);
});
test("code c1 a runner that cannot read its job list and cannot restart refuses with its notice",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd,LIST_FAILS),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);h.setJobs([verifiedJob()]);failList(h);writeFileSync(join(r.cwd,"untracked.txt"),"x");
 const err=await h.poll(gate)+await h.poll(gate);
 assert.equal(execs.length,0);assert.equal(r.head(),r.a);assert.equal(h.claims().length,0);assert.equal(h.record().state,"refused");assert.equal(h.record().reason,"checkout-dirty");
 assert.deepEqual(codeSends(h).map(s=>s.requestId.split("-").slice(5).join("-")),["refused-checkout-dirty"]);assert.ok(err.startsWith(REFUSED_LINE+"Deployment poll held ("),err);
 // An unreadable published ref stays silent, as when a ledger was read.
 const plain=mkdtempSync(join(tmpdir(),"release-code-plain-"));t.after(()=>rmSync(plain,{recursive:true,force:true}));
 const q=codeHost(t,plain,LIST_FAILS);q.setJobs([verifiedJob()]);failList(q);const quiet=codeGate(r.codeA);
 assert.match(await q.poll(quiet.gate),/^Deployment poll held \(/);assert.deepEqual(codeSends(q),[]);assert.equal(q.record().reason,"published-unreadable");assert.equal(quiet.execs.length,0);
});
// The published runner's load check (wi_56ac00ba2854fa3d c2).
const RESTORED_LINE="Deployer code is out of date and the published runner does not load; its checkout is back on the loaded commit and no release is claimed until a published runner loads.\n";
const UNRESTORED_LINE="Deployer code is out of date and the published runner does not load; its checkout could not be restored, so do not re-provision from it: restore it to the loaded commit or fix the published scripts.\n";
test("code c2 a published runner that does not load is refused once, with no exec, its checkout is restored, and it is not retried for that digest",{timeout:60000},async t=>{
 for(const mode of ["throw","hang"]){
  const r=codeRepo(t),h=codeHost(t,r.cwd),counter=join(h.home,"loads-"+mode),bad=codeCommit(r,r.a,unloadable(counter,mode));r.at(r.a,bad.commit);h.setJobs([verifiedJob()]);
  const {gate,execs}=codeGate(r.codeA,{loadTimeoutMs:300}),started=Date.now(),heads=[];gate.loads=(load=>function(cwd,signal){heads.push(git(cwd,"rev-parse","HEAD"));return load.call(this,cwd,signal);})(gate.loads);
  const err=await h.poll(gate)+await h.poll(gate)+await h.poll(gate);
  assert.ok(Date.now()-started<15000,mode);
  assert.equal(readFileSync(counter,"utf8"),"x",mode);assert.equal(execs.length,0,mode);assert.equal(h.claims().length,0,mode);
  // The load check ran on the published commit; the checkout is back on the loaded one, clean, and later polls leave it there.
  assert.deepEqual(heads,[bad.commit],mode);assert.equal(r.head(),r.a,mode);assert.equal(git(r.cwd,"status","--porcelain"),"",mode);assert.equal(diskCode(join(r.cwd,"scripts")).digest,r.codeA.digest,mode);
  assert.equal(h.record().state,"refused",mode);assert.equal(h.record().reason,"load-failed",mode);assert.equal(h.record().checkout,"restored",mode);assert.equal(h.record().loadedCommit,r.a,mode);
  assert.deepEqual(codeSends(h).map(s=>s.requestId),[`runner-code-${CODE_RUN}-${r.codeA.digest.slice(0,12)}-${bad.code.digest.slice(0,12)}-refused-load-failed`],mode);
  const sent=codeSends(h)[0];assert.equal(sent.subject,"Deployer is not claiming releases: its scripts are out of date",mode);
  assert.match(sent.text,/its checkout is back on the loaded commit [a-f0-9]{12}\. Reason: load-failed\. Action: fix the published scripts on tasks-hub/,mode);assert.ok(sent.text.includes(r.a.slice(0,12)),mode);
  assert.doesNotMatch(sent.text,/tt deployment setup|re-provision/,mode);assert.match(sent.text,SAFE_TEXT,mode);
  assert.equal(err,RESTORED_LINE,mode);
  // A new published digest is checked again, and restarts once it loads.
  const fixed=codeCommit(r,r.a,"// release-runner.mjs fixed\n");r.at(r.head(),fixed.commit);
  await h.poll(gate);assert.equal(execs.length,1,mode);assert.equal(r.head(),fixed.commit,mode);assert.equal(h.record().state,"restart",mode);assert.equal(readFileSync(counter,"utf8"),"x",mode);
  assert.deepEqual(codeSends(h).map(s=>s.requestId.split("-").slice(5).join("-")),["refused-load-failed","restart"],mode);
 }
 assert.ok(CODE_REASONS.includes("load-failed"));
});
test("code c2 a runner whose own release published scripts that do not load restores the commit it loaded from",{timeout:60000},async t=>{
 // The usual case: its release left HEAD and tasks-hub on the new commit, so the restart's prepare does not move HEAD.
 const r=codeRepo(t),h=codeHost(t,r.cwd),counter=join(h.home,"loads"),bad=codeCommit(r,r.a,unloadable(counter,"throw"));r.at(r.a,r.a);
 const {gate,execs}=codeGate(r.codeA,{loadTimeoutMs:300});
 await h.poll(gate);assert.equal(h.record().state,"current");assert.equal(codeSends(h).length,0);
 r.at(bad.commit,bad.commit);h.setJobs([verifiedJob()]);
 const err=await h.poll(gate)+await h.poll(gate);
 assert.equal(readFileSync(counter,"utf8"),"x");assert.equal(execs.length,0);assert.equal(h.claims().length,0);
 assert.equal(r.head(),r.a);assert.equal(git(r.cwd,"status","--porcelain"),"");assert.equal(git(r.cwd,"rev-parse","refs/heads/tasks-hub"),bad.commit);
 assert.equal(h.record().checkout,"restored");assert.equal(h.record().loadedCommit,r.a);
 assert.deepEqual(codeSends(h).map(s=>s.requestId.split("-").slice(5).join("-")),["refused-load-failed"]);assert.equal(err,RESTORED_LINE);
});
test("code c2 a load-failed refusal whose checkout cannot be restored says not to re-provision from it",{timeout:60000},async t=>{
 // Two real restore failures: the checkout is left dirty, or the HEAD the first poll found does not hold the loaded scripts.
 const cases={dirty:(r,bad)=>{r.at(r.a,bad.commit);return cwd=>writeFileSync(join(cwd,"stray.txt"),"x");},moved:(r,bad)=>{r.at(r.b,bad.commit);return ()=>{};}};
 for(const [name,setup] of Object.entries(cases)){
  const r=codeRepo(t),h=codeHost(t,r.cwd),counter=join(h.home,"loads"),bad=codeCommit(r,r.a,unloadable(counter,"throw")),after=setup(r,bad);h.setJobs([verifiedJob()]);
  const {gate,execs}=codeGate(r.codeA,{loadTimeoutMs:300});gate.loads=(load=>async function(cwd,signal){const out=await load.call(this,cwd,signal);after(cwd);return out;})(gate.loads);
  const err=await h.poll(gate);const head=r.head();
  const again=await h.poll(gate);
  assert.equal(readFileSync(counter,"utf8"),"x",name);assert.equal(execs.length,0,name);assert.equal(h.claims().length,0,name);
  // Left where the failed load check put it, and not moved by the later poll.
  assert.equal(head,bad.commit,name);assert.equal(r.head(),head,name);
  const record=h.record();assert.equal(record.state,"refused",name);assert.equal(record.reason,"load-failed",name);assert.equal(record.checkout,"not-restored",name);assert.equal(record.loadedCommit,name==="moved"?r.b:r.a,name);
  const sends=codeSends(h);assert.deepEqual(sends.map(s=>s.requestId),[`runner-code-${CODE_RUN}-${r.codeA.digest.slice(0,12)}-${bad.code.digest.slice(0,12)}-refused-load-failed-not-restored`],name);
  assert.equal(sends[0].subject,"Deployer is not claiming releases: do not re-provision it from its checkout",name);
  assert.match(sends[0].text,/Reason: load-failed\. Do not re-provision from this checkout\. Action: fix the published scripts on tasks-hub; .*, or restore the checkout to the loaded commit before any re-provision/,name);
  assert.doesNotMatch(sends[0].text,/tt deployment setup/,name);assert.match(sends[0].text,SAFE_TEXT,name);assert.ok(!/[a-f0-9]{13}/.test(sends[0].text),name);
  assert.equal(err,UNRESTORED_LINE,name);assert.equal(again,"",name);
 }
 // restoreCode alone: it refuses a commit that is not a sha, or loaded code that is not valid.
 const r=codeRepo(t);r.at(r.b,r.b);
 for(const [head,loaded] of [[null,r.codeA],["x",r.codeA],[r.a,null]])assert.throws(()=>restoreCode(r.cwd,head,loaded),/not-restored/);
 // A commit whose watched blobs are not the loaded bytes is not checked out.
 assert.throws(()=>restoreCode(r.cwd,r.b,r.codeA),/not-restored/);assert.throws(()=>restoreCode(r.cwd,r.a,r.codeB),/not-restored/);
 assert.equal(r.head(),r.b);restoreCode(r.cwd,r.a,r.codeA);assert.equal(r.head(),r.a);assert.equal(git(r.cwd,"status","--porcelain"),"");
});
test("code c2 the load check imports the published runner without running its entry",{timeout:60000},async t=>{
 // A tt on PATH that logs any call: the import must make none.
 const bin=mkdtempSync(join(tmpdir(),"release-load-bin-"));t.after(()=>rmSync(bin,{recursive:true,force:true}));
 const log=join(bin,"calls");writeFileSync(join(bin,"tt"),`#!/bin/sh\necho "$@" >> ${JSON.stringify(log)}\n`);chmodSync(join(bin,"tt"),0o755);
 const path=process.env.PATH;process.env.PATH=bin+":"+path;t.after(()=>{process.env.PATH=path;});
 const r=codeRepo(t,true);r.at(r.b,r.b);
 const before=readdirSync(join(r.cwd,"scripts")).sort();
 assert.equal(await runnerLoads(r.cwd,{timeoutMs:20000}),true);
 assert.equal(existsSync(log),false);assert.equal(git(r.cwd,"status","--porcelain"),"");assert.deepEqual(readdirSync(join(r.cwd,"scripts")).sort(),before);assert.deepEqual(readdirSync(bin).sort(),["tt"]);
 // It really imports: a watched file that fails to parse fails the check, and a missing runner does too.
 writeFileSync(join(r.cwd,"scripts","release-probe.mjs"),readFileSync(join(r.cwd,"scripts","release-probe.mjs"),"utf8")+"\n)(\n");
 assert.equal(await runnerLoads(r.cwd,{timeoutMs:20000}),false);
 assert.equal(await runnerLoads(join(r.cwd,"absent"),{timeoutMs:20000}),false);
 // A stop signal kills a child that never settles.
 const hung=codeRepo(t),counter=join(bin,"hung"),slow=codeCommit(hung,hung.a,unloadable(counter,"hang"));hung.at(slow.commit,slow.commit);
 const halt=new AbortController(),began=Date.now(),pending=runnerLoads(hung.cwd,{timeoutMs:60000,signal:halt.signal});
 for(const end=Date.now()+20000;!existsSync(counter)&&Date.now()<end;)await new Promise(r=>setTimeout(r,10));
 halt.abort();assert.equal(await pending,false);assert.ok(Date.now()-began<20000);
 // An idle restart over stand-in scripts checks the load, then re-executes once; a stop during the check ends the runner with no exec.
 const s=codeRepo(t),h=codeHost(t,s.cwd),order=[];s.at(s.a,s.b);h.setJobs([verifiedJob()]);
 const {gate,execs}=codeGate(s.codeA,{execve:(...args)=>{execs.push(args);order.push("exec");}}),loads=gate.loads;gate.loads=function(cwd){order.push("load:"+git(cwd,"rev-parse","HEAD"));return loads.call(this,cwd);};
 await h.poll(gate);assert.deepEqual(order,["load:"+s.b,"exec"]);assert.equal(execs.length,1);assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer is restarting itself onto the published scripts"]);
 const u=codeRepo(t),k=codeHost(t,u.cwd),stop=new AbortController();u.at(u.a,u.b);k.setJobs([verifiedJob()]);
 const halted=codeGate(u.codeA);halted.gate.loads=async()=>{stop.abort();return true;};
 assert.equal(await k.poll(halted.gate,undefined,{signal:stop.signal}),"");assert.equal(halted.execs.length,0);assert.equal(k.claims().length,0);assert.deepEqual(k.sends(),[]);
});
// A request id per restart (wi_56ac00ba2854fa3d c5). Each gate stands in for
// one process of the run, with what the re-exec environment would give it.
const reexecGate=(from,to,generation,over={})=>codeGate(to,{marker:to.digest,restartedFrom:from.digest,restartGeneration:generation,...over});
const short12=c=>c.digest.slice(0,12),idFor=(l,c,kind)=>`runner-code-${CODE_RUN}-${short12(l)}-${short12(c)}-${kind}`;
test("code c5 each restart in a run has its own request id: A to B to C to B",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd),C=codeCommit(r,r.b,"// release-runner.mjs c\n"),A=r.codeA,B=r.codeB;r.at(r.a,r.b);
 const execs=[];const spy=over=>({...over,execve:(...args)=>{execs.push(args[2].TAILTERM_RUNNER_REEXEC_GEN);}});
 // A to B (generation 1), then the process on B.
 await h.poll(codeGate(A,spy()).gate);assert.equal(r.head(),r.b);
 const onB=reexecGate(A,B,1,spy()).gate;await h.poll(onB);
 // B to C (generation 2), then the process on C.
 r.at(r.head(),C.commit);await h.poll(onB);assert.equal(r.head(),C.commit);
 const onC=reexecGate(B,C.code,2,spy()).gate;await h.poll(onC);
 // C back to B (generation 3), then the process on B again.
 r.at(r.head(),r.b);await h.poll(onC);assert.equal(r.head(),r.b);
 await h.poll(reexecGate(C.code,B,3,spy()).gate);
 const ids=codeSends(h).map(s=>s.requestId);
 assert.deepEqual(ids,[idFor(A,B,"restart"),idFor(B,B,"restarted"),idFor(B,C.code,"restart-r2"),idFor(C.code,C.code,"restarted-r2"),idFor(C.code,B,"restart-r3"),idFor(B,B,"restarted-r3")]);
 assert.equal(new Set(ids).size,ids.length);assert.deepEqual(execs,["1","2","3"]);
 // The hub's liveness sweep matches these subjects, which do not change.
 assert.deepEqual(codeSends(h).map(s=>s.subject),Array(3).fill(["Deployer is restarting itself onto the published scripts","Deployer now runs the published scripts"]).flat());
 assert.equal(h.record().state,"current");assert.equal(h.record().restartGeneration,3);
 for(const id of ids)assert.match(id,/^[A-Za-z0-9_-]{1,128}$/);
});
test("code c5 a retry of the same restart repeats its id, and a repeated A to B after a revert gets a new one",async t=>{
 // Two polls of one process restart onto the same published code: one id, sent once, the same generation in both execs.
 {
  const r=codeRepo(t),h=codeHost(t,r.cwd),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);
  await h.poll(gate);await h.poll(gate);
  assert.equal(execs.length,2);assert.deepEqual(execs.map(e=>e[2].TAILTERM_RUNNER_REEXEC_GEN),["1","1"]);
  assert.deepEqual(codeSends(h).map(s=>s.requestId),[idFor(r.codeA,r.codeB,"restart")]);assert.equal(h.record().restartGeneration,1);
 }
 // A to B, B to A after a revert, then A to B again.
 const r=codeRepo(t),h=codeHost(t,r.cwd),A=r.codeA,B=r.codeB;r.at(r.a,r.b);
 await h.poll(codeGate(A).gate);
 const onB=reexecGate(A,B,1);r.at(r.head(),r.a);await h.poll(onB.gate);assert.equal(r.head(),r.a);
 const onA=reexecGate(B,A,2);r.at(r.head(),r.b);await h.poll(onA.gate);assert.equal(r.head(),r.b);
 assert.deepEqual(onA.execs.map(e=>e[2].TAILTERM_RUNNER_REEXEC_GEN),["3"]);
 const ids=codeSends(h).map(s=>s.requestId);
 assert.deepEqual(ids,[idFor(A,B,"restart"),idFor(B,A,"restart-r2"),idFor(A,B,"restart-r3")]);assert.notEqual(ids[2],ids[0]);assert.equal(new Set(ids).size,ids.length);
});
test("code c5 draining and refused ids are unchanged and the generation survives a new process of the same run",async t=>{
 const r=codeRepo(t),home=mkdtempSync(join(tmpdir(),"release-code-gen-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const saved=[process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN];process.env.TAILTERM_AGENT=CODE_AGENT;process.env.TAILTERM_RUN=CODE_RUN;
 t.after(()=>{for(const [i,k] of ["TAILTERM_AGENT","TAILTERM_RUN"].entries()){if(saved[i]===undefined)delete process.env[k];else process.env[k]=saved[i];}});
 const at=(decision,generation)=>codeRecord({journalDirectory:home},{loaded:r.codeA,marker:null,restartedFrom:null,restartGeneration:generation,now:()=>0},decision,r.codeB);
 const draining=at({state:"draining"},2),refused=at({state:"refused",reason:"exec-failed"},2);
 assert.equal(draining.restartGeneration,2);assert.equal(codeNotice(draining).requestId,idFor(r.codeA,r.codeB,"draining"));assert.equal(codeNotice(refused).requestId,idFor(r.codeA,r.codeB,"refused-exec-failed"));
 assert.equal(codeNotice(at({state:"restart"},2)).requestId,idFor(r.codeA,r.codeB,"restart-r2"));assert.equal(codeNotice(at({state:"restart",heldJob:"rel_held"},2)).requestId,idFor(r.codeA,r.codeB,"restart-r2-held-rel_held"));
 // The first restart and no restart at all carry no suffix and no field.
 rmSync(join(home,"runner-code.json"));assert.equal(codeNotice(at({state:"restart"},1)).requestId,idFor(r.codeA,r.codeB,"restart"));
 rmSync(join(home,"runner-code.json"));assert.equal("restartGeneration" in at({state:"draining"},null),false);
 for(const bad of [0,-1,1.5,"2",1000000,null])assert.equal(codeNotice({...at({state:"restart"},null),restartGeneration:bad}).requestId,idFor(r.codeA,r.codeB,"restart"),String(bad));
 // A new process of the run, with no re-exec environment, over a record at generation 2: its restart is the third.
 const s=codeRepo(t),h=codeHost(t,s.cwd);s.at(s.a,s.b);
 writeFileSync(join(h.home,"runner-code.json"),JSON.stringify({version:1,agentId:CODE_AGENT,runId:CODE_RUN,state:"current",restartGeneration:2}));
 const fresh=codeGate(s.codeA);await h.poll(fresh.gate);
 assert.deepEqual(codeSends(h).map(x=>x.requestId),[idFor(s.codeA,s.codeB,"restart-r3")]);assert.equal(fresh.execs[0][2].TAILTERM_RUNNER_REEXEC_GEN,"3");assert.equal(h.record().restartGeneration,3);
 // A record of another run starts again at the first restart.
 const u=codeRepo(t),k=codeHost(t,u.cwd);u.at(u.a,u.b);
 writeFileSync(join(k.home,"runner-code.json"),JSON.stringify({version:1,agentId:CODE_AGENT,runId:"run_other",state:"current",restartGeneration:5}));
 const other=codeGate(u.codeA);await k.poll(other.gate);
 assert.deepEqual(codeSends(k).map(x=>x.requestId),[idFor(u.codeA,u.codeB,"restart")]);assert.equal(other.execs[0][2].TAILTERM_RUNNER_REEXEC_GEN,"1");
 // The gate reads the generation from the re-exec environment; a restart begun without one is the first.
 const env=["TAILTERM_RUNNER_REEXEC_GEN","TAILTERM_RUNNER_REEXEC_FROM"].map(k=>[k,process.env[k]]);
 t.after(()=>{for(const [k,v] of env){if(v===undefined)delete process.env[k];else process.env[k]=v;}});
 process.env.TAILTERM_RUNNER_REEXEC_FROM=r.codeA.digest;
 for(const [value,expected] of [["4",4],["999999",999999],["0",1],["07",1],["abc",1],["1000000",1],[undefined,1]]){if(value===undefined)delete process.env.TAILTERM_RUNNER_REEXEC_GEN;else process.env.TAILTERM_RUNNER_REEXEC_GEN=value;assert.equal(runnerCodeGate().restartGeneration,expected,String(value));}
 delete process.env.TAILTERM_RUNNER_REEXEC_FROM;process.env.TAILTERM_RUNNER_REEXEC_GEN="4";assert.equal(runnerCodeGate().restartGeneration,4);
 delete process.env.TAILTERM_RUNNER_REEXEC_GEN;assert.equal(runnerCodeGate().restartGeneration,null);
});
