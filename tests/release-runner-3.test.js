import test from "node:test";
import {createServer} from "node:http";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,copyFileSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,readdirSync,rmSync,realpathSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname,resolve} from "node:path";
import {execFileSync,execFile,spawnSync} from "node:child_process";
import {refusalText,heldClaimNotice,integrateCandidate,runRelease,HostAdapter,serveDeployment,releaseError,compatibilityArgv,dispatchCompatibility,validateNativeRelease,failureReason,failureDetail,matrixWaitNotice,RUNNER_CODE_FILES,LOADED_CODE,CODE_REASONS,codeDigest,diskCode,publishedCode,codeDecision,prepareCode,runnerCodeGate,codeRecord,codeNotice,cliFailureNotice,cliRetry,transportWait,transportWaitNotice} from "../scripts/release-runner.mjs";
import {acquireHostLock,readHostState as rawReadHostState,holdersOf} from "../scripts/verify-matrix-host-lock.mjs";
import {createHash} from "node:crypto";
import {releaseReply,releaseReplySource,git,bufferTT,fixture,change,job,fake,config,hash,stamped,importInputs,hostFixture,recorded,C,remoteHead,RUNNER,attemptDir,until,ended,idle,lockHost,reasonIs,MATRIX_A,uncoveredReason,matrixChangeRelease,READY_ID,READY_UP,READY_CAPTURE,READY_DOWN,readyAdapter,EXIT1,countedLiveCheck,readyRelease,TOKEN,NEXT_ACTION,conflictFixture,journalOf,refusesUnchanged,CODE_AGENT,CODE_RUN,SAFE_TEXT,codeRepo,codeHost,verifiedJob,codeGate,codeSends,HELD_ITEM,HELD_SECRET,HELD_HANDLER,HELD_HELPER,heldRoster,sendOf,heldRun,unwatchedImports,heldOwn,heldJournal,HELD_SUBJECT,three,race,covers,planned,stale,claimed} from "./release-runner-fixtures.mjs";
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
 assert.equal(err,"Release claim held; handler reconciliation required.\nHeld claim notice not delivered to every recipient; the next poll retries.\n".repeat(3));
 const sent=h.calls().filter(c=>c.argv[0]==="send").map(sendOf);assert.equal(sent.length,1);assert.equal(sent[0].to,null);assert.match(sent[0].requestId,/-board$/);
 assert.ok(sent[0].text.includes("Reason: hub: 409 conflict: release: job cannot be taken over (tt deployment claim exit 1)."));
 assert.equal(h.claims().filter(c=>c.argv.includes("rel_next")).length,3);
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
test("held s2 a stale runner whose only owned job is held restarts once, at the published commit, before any claim or release run",async t=>{
 for(const journal of [{},{refuseFailure:undefined},{phase:"prepared",refusalReason:undefined,refuseFailure:undefined},{phase:"integrated",refusalReason:undefined,refuseFailure:undefined,integrated:"d".repeat(40),expected:"e".repeat(40)},{published:false}]){
  const name=JSON.stringify(journal),r=codeRepo(t),h=codeHost(t,r.cwd),own=heldOwn(),released=[],seen=[];r.at(r.a,r.b);h.setJobs([own,verifiedJob("rel_next")]);heldJournal(h,own,journal);
  const before=readFileSync(join(h.home,"rel_held.json"),"utf8");
  const {gate,execs}=codeGate(r.codeA,{execve:(...args)=>{execs.push(args);seen.push({record:h.record(),head:r.head(),claims:h.claims().length,released:released.length,sends:codeSends(h).length});}});
  const err=await h.poll(gate,async c=>{released.push(c.job.id);});
  assert.equal(execs.length,1,name);assert.deepEqual(seen[0],{record:h.record(),head:r.b,claims:0,released:0,sends:1},name);
  assert.equal(h.record().state,"restart",name);assert.equal(h.record().heldJob,"rel_held",name);assert.equal(h.record().loaded.digest,r.codeA.digest,name);assert.equal(h.record().current.digest,r.codeB.digest,name);
  const [file,argv,env]=execs[0];assert.equal(file,process.execPath);assert.deepEqual(argv,[process.execPath,...process.execArgv,...process.argv.slice(1)]);
  assert.equal(env.TAILTERM_AGENT,CODE_AGENT);assert.equal(env.TAILTERM_RUN,CODE_RUN);assert.equal(env.TAILTERM_RUNNER_REEXEC,r.codeB.digest);assert.equal(env.TAILTERM_RUNNER_REEXEC_FROM,r.codeA.digest);
  // The held job's journal is left exactly as it was, and nothing is claimed.
  assert.equal(readFileSync(join(h.home,"rel_held.json"),"utf8"),before,name);assert.equal(h.claims().length,0,name);
  const sent=codeSends(h);assert.deepEqual(sent.map(s=>s.subject),[HELD_SUBJECT],name);
  assert.match(sent[0].text,/Its only owned release rel_held is held, .* so it restarts itself now with that held job and the same agent and run/);assert.match(sent[0].text,SAFE_TEXT);
  assert.equal(sent[0].requestId,`runner-code-${CODE_RUN}-${r.codeA.digest.slice(0,12)}-${r.codeB.digest.slice(0,12)}-restart-held-rel_held`);
  assert.match(err,/^Deployer restarting itself onto the published scripts\.\n/,name);
 }
});

// Transport retry (wi_9cb499ac2a14a1a0). A fake hub CLI over one claimed job
// kept in a file: it answers get, check, handler, merged, refuse, finish and
// send, logs every call, and fails a call with a rule's stderr and exit 1 when
// the call is the from..to th of its operation (to null: every later one). A
// send's operation is "send:" and its subject. The retry's wait is recorded
// instead of slept.
const DEADLINE='tt: Post "http://hub.invalid/api/tasks/tsk_fixture/messages": context deadline exceeded',INPUTS_SEND="send:Import immutable inputs for this release job",MATRIX_SEND="send:Import verification for the integrated release commit";
function retryHost(t,rules=[]){
 const f=fixture(),commit=change(f,"client/a.js","a"),home=mkdtempSync(join(tmpdir(),"release-retry-"));t.after(()=>{for(const d of [home,f.cwd,f.origin])rmSync(d,{recursive:true,force:true});});
 const tt=join(home,"tt"),log=join(home,"calls.jsonl"),jobPath=join(home,"job.json"),faultPath=join(home,"faults.json");
 const claimed={...job(f,commit),id:"rel_retry",itemId:"wi_0123456789abcdef",itemRevision:1,orderMessageSeq:7,generation:2,agentId:"agt_fixture",runId:"run_fixture"};
 const fault=next=>writeFileSync(faultPath,JSON.stringify({rules:next,seen:{}}));fault(rules);writeFileSync(jobPath,JSON.stringify(claimed));
 writeFileSync(tt,"#!"+process.execPath+`\nconst fs=require('fs'),a=process.argv.slice(2),flag=n=>a[a.indexOf(n)+1],op=a[0]==='send'?'send:'+flag('--subject'):a[1]||a[0];
const faults=JSON.parse(fs.readFileSync(${JSON.stringify(faultPath)},'utf8')),n=faults.seen[op]=(faults.seen[op]||0)+1;fs.writeFileSync(${JSON.stringify(faultPath)},JSON.stringify(faults));
fs.appendFileSync(${JSON.stringify(log)},JSON.stringify({op,argv:a})+'\\n');
const rule=faults.rules.find(r=>r.op===op&&n>=r.from&&(r.to===null||n<=r.to));if(rule){fs.writeSync(2,rule.stderr+'\\n');process.exit(1);}
const job=JSON.parse(fs.readFileSync(${JSON.stringify(jobPath)},'utf8')),save=()=>{fs.writeFileSync(${JSON.stringify(jobPath)},JSON.stringify(job));console.log(JSON.stringify(job));};
if(a[0]==='send')process.exit(0);
if(a[1]==='handler')console.log(JSON.stringify({id:'agt_0123abcd'}));
else if(['get','check'].includes(a[1]))console.log(JSON.stringify(job));
else if(a[1]==='merged'){job.state='merged';job.generation++;job.integratedCommit=flag('--commit');save();}
else if(a[1]==='refuse'){job.state='refused';job.generation++;save();}
else if(a[1]==='finish'){const r=JSON.parse(fs.readFileSync(flag('--file'),'utf8'));job.state=r.outcome;job.generation++;job.receipt=r;save();}
else process.exit(2);`);chmodSync(tt,0o755);
 const slept=[],sleep=cliRetry.sleep;cliRetry.sleep=ms=>{slept.push(ms);};t.after(()=>{cliRetry.sleep=sleep;});
 const saved=()=>JSON.parse(readFileSync(jobPath,"utf8"));
 // A poll's adapter: the real CLI paths, with the target steps stubbed.
 const adapter=()=>Object.assign(new HostAdapter({cwd:f.cwd,tt,journalDirectory:home},saved()),{prepare:async()=>({release:"fixture-release",artifactSHA256:"b".repeat(64)}),deploy:async()=>{},check:async()=>true,rollback:async()=>true,retain:async()=>{},jobInputs:()=>({targets:{}})});
 const calls=()=>existsSync(log)?readFileSync(log,"utf8").trim().split("\n").filter(Boolean).map(l=>JSON.parse(l)):[];
 const ops=name=>calls().filter(c=>c.op===name),journal=join(home,"rel_retry.json");
 const release=a=>runRelease({...config(f,claimed),journalPath:journal},a);
 const failures=()=>JSON.parse(readFileSync(join(home,"cli-failures.json"),"utf8")).failures;
 return {f,commit,home,tt,claimed,fault,saved,bind:fields=>writeFileSync(jobPath,JSON.stringify({...saved(),...fields})),adapter,calls,ops,slept,journal,state:()=>JSON.parse(readFileSync(journal,"utf8")),release,failures};
}
const requestId=c=>c.argv[c.argv.indexOf("--request-id")+1];
test("retry t4 a request that times out once is sent again and the release goes on: nothing is refused and one failure is recorded",async t=>{
 const h=retryHost(t,[{op:INPUTS_SEND,from:1,to:1,stderr:DEADLINE}]),first=h.adapter();
 assert.deepEqual(await h.release(first),{jobId:"rel_retry",outcome:"waiting_inputs"});
 // The same keyed request twice, one wait between, and nothing left to report.
 const sent=h.ops(INPUTS_SEND);assert.equal(sent.length,2);assert.equal(requestId(sent[0]),`rel_retry-g2-inputs-${h.commit}`);assert.equal(requestId(sent[1]),requestId(sent[0]));
 assert.deepEqual(h.slept,[cliRetry.delaysMs[0]]);assert.deepEqual(first.cliFailures,[]);assert.equal(h.state().phase,"waiting_inputs");
 let kept=h.failures();assert.equal(kept.length,1);assert.equal(kept[0].count,1);assert.equal(kept[0].exitCode,1);assert.equal(kept[0].stderr,DEADLINE+"\n");assert.equal(kept[0].jobId,"rel_retry");
 // The handler binds the inputs; the next poll publishes and finishes.
 h.bind({inputsCommit:h.commit,inputsDigest:"d".repeat(64)});
 const receipt=await h.release(h.adapter());assert.equal(receipt.outcome,"released");assert.equal(receipt.commit,h.commit);
 assert.equal(h.saved().state,"released");assert.equal(h.state().phase,"complete");assert.equal(git(h.f.cwd,"rev-parse","refs/heads/tasks-hub"),h.commit);assert.equal(remoteHead(h.f),h.commit);
 assert.equal(h.ops("refuse").length,0);assert.equal(h.ops("merged").length,1);assert.equal(h.ops("finish").length,1);assert.equal(h.ops("send:Release refused before publication").length,0);
 assert.equal(h.state().refusalReason,undefined);assert.deepEqual(h.slept,[cliRetry.delaysMs[0]]);kept=h.failures();assert.equal(kept.length,1);assert.equal(kept[0].count,1);
});
test("fence r1 an unpublished job whose fence check spends its transport tries waits for the next poll and is not refused",async t=>{
 // A new job: one good check, then three tries of the next. No journal is left, nothing is refused, the claim stands.
 const h=retryHost(t,[{op:"check",from:2,to:null,stderr:DEADLINE}]),a=h.adapter(),reason="tt deployment check timed out: context deadline exceeded, 3 tries",waits={jobId:"rel_retry",outcome:"waiting_transport"};
 const untouched=()=>{assert.equal(h.ops("refuse").length,0);assert.equal(h.ops("merged").length,0);assert.equal(h.ops("send:Release refused before publication").length,0);assert.equal(h.saved().state,"claimed");assert.equal(git(h.f.cwd,"rev-parse","refs/heads/tasks-hub"),h.f.base);};
 assert.deepEqual(await h.release(a),waits);
 assert.equal(h.ops("check").length,4);assert.deepEqual(h.slept,cliRetry.delaysMs);assert.equal(new Set(h.ops("check").slice(1).map(requestId)).size,1);
 assert.equal(existsSync(h.journal),false);untouched();assert.deepEqual(a.cliFailures.map(x=>[x.reason,x.transport]),[[reason,true]]);
 const kept=h.failures();assert.equal(kept.length,1);assert.equal(kept[0].count,3);
 // The poll's first check, before anything is read or locked, waits the same way.
 h.fault([{op:"check",from:1,to:null,stderr:"tt: hub: 503 Service Unavailable"}]);assert.deepEqual(await h.release(h.adapter()),waits);assert.equal(existsSync(h.journal),false);untouched();
 // The hub answers again: the same job goes on to its inputs wait.
 h.fault([]);assert.deepEqual(await h.release(h.adapter()),{jobId:"rel_retry",outcome:"waiting_inputs"});assert.equal(h.state().phase,"waiting_inputs");
 // A resumed wait keeps its journal byte for byte when the first check is unreached.
 const before=readFileSync(h.journal,"utf8");
 h.fault([{op:"check",from:1,to:null,stderr:DEADLINE}]);assert.deepEqual(await h.release(h.adapter()),waits);assert.equal(readFileSync(h.journal,"utf8"),before);untouched();
 // Inputs bound; the check just before publication is unreached: still unpublished, resumable, unrefused.
 h.bind({inputsCommit:h.commit,inputsDigest:"d".repeat(64)});
 h.fault([{op:"check",from:3,to:null,stderr:"tt: dial tcp 192.0.2.1:18765: connect: connection refused"}]);assert.deepEqual(await h.release(h.adapter()),waits);
 assert.equal(h.ops("check").filter((c,i,all)=>i>=all.length-3).length,3);let state=h.state();assert.equal(state.phase,"waiting_inputs");assert.notEqual(state.published,true);assert.deepEqual(state.effects,[]);assert.equal(state.refusalReason,undefined);assert.equal(state.failure,undefined);untouched();
 // The next poll publishes once and finishes.
 h.fault([]);const receipt=await h.release(h.adapter());assert.equal(receipt.outcome,"released");assert.equal(receipt.commit,h.commit);
 assert.equal(h.ops("merged").length,1);assert.equal(h.ops("finish").length,1);assert.equal(h.ops("refuse").length,0);assert.equal(h.saved().state,"released");assert.equal(git(h.f.cwd,"rev-parse","refs/heads/tasks-hub"),h.commit);assert.equal(remoteHead(h.f),h.commit);
});
test("fence r1 a re-read after a generation change that cannot reach the hub leaves the unpublished job waiting, and any other failed re-read is a lost fence",async t=>{
 const CHANGED="tt: hub: 409 conflict: release: generation changed";
 // The check is answered with a moved generation, then the re-read spends its three tries: the job waits, unrefused, with no journal.
 const h=retryHost(t,[{op:"check",from:2,to:2,stderr:CHANGED},{op:"get",from:1,to:null,stderr:DEADLINE}]),a=h.adapter();
 assert.deepEqual(await h.release(a),{jobId:"rel_retry",outcome:"waiting_transport"});
 assert.equal(h.ops("check").length,2);assert.equal(h.ops("get").length,cliRetry.tries);assert.deepEqual(h.slept,cliRetry.delaysMs);assert.equal(existsSync(h.journal),false);
 assert.equal(h.ops("refuse").length,0);assert.equal(h.ops("send:Release refused before publication").length,0);assert.equal(h.saved().state,"claimed");assert.equal(git(h.f.cwd,"rev-parse","refs/heads/tasks-hub"),h.f.base);
 assert.deepEqual(a.cliFailures.filter(x=>x.transport).map(x=>x.reason),["tt deployment get timed out: context deadline exceeded, 3 tries"]);
 // The next poll, with the hub back, goes on to the inputs wait.
 h.fault([]);assert.deepEqual(await h.release(h.adapter()),{jobId:"rel_retry",outcome:"waiting_inputs"});assert.equal(h.ops("refuse").length,0);
 // A re-read the hub refuses is a lost fence as before: one read, refused.
 const r=retryHost(t,[{op:"check",from:2,to:2,stderr:CHANGED},{op:"get",from:1,to:null,stderr:"tt: hub: 500 internal: database is locked"}]);
 await assert.rejects(r.release(r.adapter()),/Release refused before publication/);
 assert.equal(r.ops("get").length,1);assert.deepEqual(r.slept,[]);assert.equal(r.state().refusalReason,"release fence lost");assert.equal(r.ops("refuse").length,1);
});
test("fence r1 a fence the hub answers still refuses, and after publication an unreached fence check fails and rolls back as before",async t=>{
 // A lost fence and a line that only says timeout are the hub's or the CLI's answer: one call, refused.
 for(const stderr of ["tt: hub: 409 conflict: release: job is not claimed","tt: timeout","tt: the request timed out"]){
  const h=retryHost(t,[{op:"check",from:2,to:null,stderr}]);
  await assert.rejects(h.release(h.adapter()),/Release refused before publication/,stderr);
  assert.equal(h.ops("check").length,2,stderr);assert.deepEqual(h.slept,[],stderr);assert.equal(h.state().refusalReason,"release fence lost",stderr);assert.equal(h.state().phase,"refused",stderr);assert.equal(h.ops("refuse").length,1,stderr);
 }
 // Published, nothing deployed yet: the check before the first target step is unreached. No wait, no refusal: the failure path.
 const reason="tt deployment check timed out: context deadline exceeded, 3 tries";
 const bound=async()=>{const h=retryHost(t);assert.deepEqual(await h.release(h.adapter()),{jobId:"rel_retry",outcome:"waiting_inputs"});h.bind({inputsCommit:h.commit,inputsDigest:"d".repeat(64)});return h;};
 const p=await bound();p.fault([{op:"check",from:4,to:null,stderr:DEADLINE}]);
 await assert.rejects(p.release(p.adapter()),/Release failed/);
 let state=p.state();assert.equal(state.published,true);assert.equal(state.failure.reason,reason);assert.equal(state.failure.step,"prepare");assert.equal(state.outcome,"blocked");assert.equal(state.revert.outcome,"committed");
 assert.equal(p.ops("merged").length,1);assert.equal(p.ops("refuse").length,0);assert.equal(p.ops("send:Release failed and requires recovery").length,1);assert.equal(p.ops("check").length,2+3+cliRetry.tries);
 // An effect is journaled, then the check before the deploy is unreached: one rollback attempt per effect, whose own fence check is tried once and caught.
 const r=await bound(),ra=r.adapter();let rollbacks=0,deploys=0;ra.rollback=async()=>{rollbacks++;return true;};ra.deploy=async()=>{deploys++;};
 r.fault([{op:"check",from:5,to:null,stderr:DEADLINE}]);
 await assert.rejects(r.release(ra),/Release failed/);
 state=r.state();assert.equal(state.published,true);assert.equal(deploys,0);assert.equal(rollbacks,0);assert.equal(state.effects.length,1);assert.equal(state.effects[0].rollbackAttempted,true);assert.equal(state.effects[0].rollback,"blocked");
 assert.equal(state.failure.reason,reason);assert.equal(state.failure.step,"deploy");assert.equal(r.ops("check").length,2+4+2*cliRetry.tries);assert.equal(r.ops("merged").length,1);assert.equal(r.ops("refuse").length,0);
 // With the hub back for the rollback's check, the same failure restores the target: the rollback path is the one it was.
 const g=await bound(),ga=g.adapter();let restored=0;ga.rollback=async()=>{restored++;return true;};
 g.fault([{op:"check",from:5,to:7,stderr:DEADLINE}]);
 await assert.rejects(g.release(ga),/Release failed/);
 assert.equal(restored,1);assert.equal(g.state().effects[0].rollback,"restored");assert.equal(g.state().outcome,"rolled_back");assert.equal(g.ops("merged").length,1);
});
test("retry t1 t4 a hub refusal is never sent again, and only an idempotent call of the configured CLI is retried",async t=>{
 // A 4xx on the fence check: one call, the fence is lost as before and nothing waits.
 const h=retryHost(t,[{op:"check",from:2,to:null,stderr:"tt: hub: 409 conflict: release: job is not claimed"}]);
 await assert.rejects(h.release(h.adapter()),/Release refused before publication/);
 assert.equal(h.ops("check").length,2);assert.deepEqual(h.slept,[]);assert.equal(h.state().refusalReason,"release fence lost");assert.equal(h.failures()[0].count,1);
 const get=[h.tt,"deployment","get","--job","rel_retry"],keyed=[h.tt,"send","--kind","notice","--subject","Keyed","--text","x","--request-id","rel_retry-keyed"];
 const tried=(rules,argv,why)=>{h.fault(rules);const a=h.adapter(),before=h.calls().length,waits=h.slept.length;let reason="";try{a.command(argv);}catch(error){reason=failureReason(error);assert.equal(error.transport===true,/ tr(y|ies)$/.test(reason),why);}
  return [h.calls().length-before,h.slept.length-waits,reason];};
 // What counts as a transport error: the fixed causes, and 502, 503 and 504 only.
 for(const [stderr,cause] of [[DEADLINE,"timed out: context deadline exceeded"],['tt: Get "http://hub.invalid/x": dial tcp 192.0.2.1:18765: connect: connection refused',"connection refused"],["tt: read tcp 192.0.2.1:1->192.0.2.2:2: read: connection reset by peer","connection reset"],["tt: dial tcp 192.0.2.1:18765: i/o timeout","timed out: i/o timeout"],['tt: Get "http://hub.invalid/x": net/http: request canceled (Client.Timeout exceeded while awaiting headers)',"timed out: Client.Timeout exceeded"],['tt: Post "http://hub.invalid/x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)',"timed out: context deadline exceeded"],["tt: hub: 502 Bad Gateway","hub answered 502"],["tt: hub: 503 Service Unavailable","hub answered 503"],["tt: hub: 504 Gateway Timeout","hub answered 504"]])
  assert.deepEqual(tried([{op:"get",from:1,to:null,stderr}],get,stderr),[3,2,`tt deployment get ${cause}, 3 tries`],stderr);
 // The hub's own answer is final whatever its words: a refusal, a validation answer, a 500.
 for(const stderr of ["tt: hub: 409 conflict: release: generation changed","tt: hub: 422 validation: subject mentions a timeout","tt: hub: 400 bad request: context deadline exceeded is not a subject","tt: hub: 404 not found","tt: hub: 500 internal: database connection reset"])
  assert.deepEqual(tried([{op:"get",from:1,to:null,stderr}],get,stderr),[1,0,"tt deployment get exit 1"],stderr);
 // r2: with no hub status, a line that only says timeout or timed out is not a transport error and is made once.
 for(const stderr of ["tt: timeout","tt: timed out","tt: the operation timed out","tt: lock wait timeout exceeded","tt: Timeout","tt: client.timeout exceeded"])
  assert.deepEqual(tried([{op:"get",from:1,to:null,stderr}],get,stderr),[1,0,"tt deployment get exit 1"],stderr);
 assert.deepEqual(tried([{op:"send:Keyed",from:1,to:null,stderr:"tt: timeout"}],keyed,"keyed timeout"),[1,0,"tt send exit 1"]);
 // r2: a call the runner stops at its own command timeout printed nothing; the hub decided nothing, so it stays a transport error.
 const hung=join(h.home,"hung");mkdirSync(hung);writeFileSync(join(hung,"tt"),"#!"+process.execPath+"\nsetTimeout(()=>{},600000);");chmodSync(join(hung,"tt"),0o755);
 const stopped=new HostAdapter({cwd:h.f.cwd,tt:join(hung,"tt")},h.saved()),waited=h.slept.length;
 assert.throws(()=>stopped.command([join(hung,"tt"),"deployment","get","--job","rel_retry"],undefined,{timeout:250}),e=>failureReason(e)==="tt deployment get no answer before the command timeout, 3 tries"&&e.transport===true);assert.equal(h.slept.length,waited+2);
 // A notice sent with retry off is made once, whatever its transport error.
 const once=h.adapter(),sent=h.calls().length;h.fault([{op:"send:Keyed",from:1,to:null,stderr:DEADLINE}]);
 assert.throws(()=>once.command(keyed,undefined,{retry:false}),e=>failureReason(e)==="tt send timed out: context deadline exceeded, 1 try"&&e.transport===true);assert.equal(h.calls().length,sent+1);
 assert.deepEqual(tried([{op:"send:Keyed",from:1,to:null,stderr:"tt: hub: 409 conflict: request id reused with other data"}],keyed,"keyed 409"),[1,0,"tt send exit 1"]);
 // A keyed send and a keyed deployment call are retried; a success ends the tries.
 assert.deepEqual(tried([{op:"send:Keyed",from:1,to:2,stderr:DEADLINE}],keyed,"keyed"),[3,2,""]);
 assert.deepEqual(tried([{op:"refuse",from:1,to:null,stderr:DEADLINE}],[h.tt,"deployment","refuse","--job","rel_retry","--generation","2","--request-id","rel_retry-refuse-2"],"native"),[3,2,"tt deployment refuse timed out: context deadline exceeded, 3 tries"]);
 // A call without a request id that is not a read is made once, and names its cause.
 assert.deepEqual(tried([{op:"send:Bare",from:1,to:null,stderr:DEADLINE}],[h.tt,"send","--kind","notice","--subject","Bare","--text","x"],"bare send"),[1,0,"tt send timed out: context deadline exceeded, 1 try"]);
 assert.deepEqual(tried([{op:"refuse",from:1,to:null,stderr:DEADLINE}],[h.tt,"deployment","refuse","--job","rel_retry","--generation","2"],"bare native"),[1,0,"tt deployment refuse timed out: context deadline exceeded, 1 try"]);
 assert.deepEqual(tried([{op:"spawn",from:1,to:null,stderr:DEADLINE}],[h.tt,"spawn"],"other subcommand"),[1,0,"tt spawn timed out: context deadline exceeded, 1 try"]);
 // Another program is never retried, whatever it prints.
 const other=h.adapter(),waits=h.slept.length;assert.throws(()=>other.command([process.execPath,"-e","process.stderr.write('connection refused\\n');process.exit(5)"]),e=>failureReason(e)==="node exit 5"&&e.transport===undefined);assert.equal(h.slept.length,waits);
 // No retry starts after the window: a wait that would pass it is not begun.
 const window=cliRetry.windowMs;t.after(()=>{cliRetry.windowMs=window;});cliRetry.windowMs=cliRetry.delaysMs[0]-1;
 assert.deepEqual(tried([{op:"get",from:1,to:null,stderr:DEADLINE}],get,"window"),[1,0,"tt deployment get timed out: context deadline exceeded, 1 try"]);
 cliRetry.windowMs=cliRetry.delaysMs[0]+2000;
 assert.deepEqual(tried([{op:"get",from:1,to:null,stderr:DEADLINE}],get,"window"),[2,1,"tt deployment get timed out: context deadline exceeded, 2 tries"]);
});
test("retry t2 a request sent at every poll waits for the next poll when its transport failure outlasts the tries",async t=>{
 // The inputs request: three tries, then the job waits, unrefused, and the next poll sends the same request.
 const h=retryHost(t,[{op:INPUTS_SEND,from:1,to:null,stderr:"tt: hub: 503 Service Unavailable"}]),a=h.adapter();
 assert.deepEqual(await h.release(a),{jobId:"rel_retry",outcome:"waiting_inputs"});
 assert.equal(h.ops(INPUTS_SEND).length,3);assert.deepEqual(h.slept,cliRetry.delaysMs);assert.equal(h.state().phase,"waiting_inputs");assert.equal(h.state().refusalReason,undefined);assert.equal(h.ops("refuse").length,0);assert.equal(h.saved().state,"claimed");
 assert.deepEqual(a.cliFailures.map(x=>[x.reason,x.transport,x.jobId]),[["tt send hub answered 503, 3 tries",true,"rel_retry"]]);
 h.fault([]);assert.deepEqual(await h.release(h.adapter()),{jobId:"rel_retry",outcome:"waiting_inputs"});
 assert.equal(h.ops(INPUTS_SEND).length,4);assert.equal(new Set(h.ops(INPUTS_SEND).map(requestId)).size,1);
 // Its reads wait the same way; a refusal of the request still fails the job's step.
 const v=h.adapter();
 h.fault([{op:"get",from:1,to:null,stderr:DEADLINE}]);assert.equal(await v.verifyInputs(h.commit),false);
 h.fault([{op:"handler",from:1,to:null,stderr:DEADLINE}]);assert.equal(await v.verifyInputs(h.commit),false);
 h.fault([{op:INPUTS_SEND,from:1,to:null,stderr:"tt: hub: 422 validation: the ask is too long"}]);const sends=h.ops(INPUTS_SEND).length;
 await assert.rejects(v.verifyInputs(h.commit),reasonIs("tt send exit 1"));assert.equal(h.ops(INPUTS_SEND).length,sends+1);
 // The integrated verification import request, from an ended run with an eligible receipt.
 const integrated={...h.claimed,integratedCommit:h.commit},dir=join(h.home,"rel_retry-integrated-verification",h.commit+"-r0");mkdirSync(dir,{recursive:true});
 writeFileSync(join(dir,"run.json"),JSON.stringify({version:1,state:"ended",reason:"receipt"}));writeFileSync(join(dir,"receipt.json"),JSON.stringify({environment:{},checks:[{exitCode:0}]}));
 const m=h.adapter();h.fault([{op:MATRIX_SEND,from:1,to:null,stderr:"tt: read tcp 192.0.2.1:1->192.0.2.2:2: read: connection reset by peer"}]);
 assert.equal(await m.verifyIntegrated(integrated),false);assert.equal(h.ops(MATRIX_SEND).length,3);assert.notEqual(m.matrixImportRequested,true);
 h.fault([{op:"get",from:1,to:null,stderr:DEADLINE}]);assert.equal(await m.verifyIntegrated(integrated),false);assert.equal(h.ops(MATRIX_SEND).length,3);
 h.fault([{op:MATRIX_SEND,from:1,to:null,stderr:"tt: hub: 403 forbidden: not the deployer"}]);await assert.rejects(m.verifyIntegrated(integrated),reasonIs("tt send exit 1"));assert.equal(h.ops(MATRIX_SEND).length,4);
 h.fault([]);assert.equal(await m.verifyIntegrated(integrated),false);assert.equal(m.matrixImportRequested,true);assert.equal(new Set(h.ops(MATRIX_SEND).map(requestId)).size,1);
 assert.equal(requestId(h.ops(MATRIX_SEND)[0]),`rel_retry-integrated-matrix-${h.commit}`);
});
test("retry t2 t3 the deployer names a waiting job's spent transport failure once, in safe words, and holds nothing",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-retry-wait-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const sleep=cliRetry.sleep,slept=[];cliRetry.sleep=ms=>{slept.push(ms);};t.after(()=>{cliRetry.sleep=sleep;});
 const h=codeHost(t,cwd,`if(a[1]==='handler'){fs.writeSync(2,'tt: Get "http://hub.invalid/'+${JSON.stringify(HELD_SECRET)}+'": dial tcp: connect: connection refused\\n');process.exit(1);}`);h.setJobs([verifiedJob("rel_wait")]);
 const waits=async(c,adapter)=>{try{adapter.handler();}catch(error){assert.equal(error.transport,true);}return {jobId:c.job.id,outcome:"waiting_inputs"};};
 const reason="tt deployment handler connection refused, 3 tries",line="Release waits: a hub call failed in transport; the next poll retries.\n";
 assert.equal(await h.poll(null,waits),line);assert.deepEqual(slept,cliRetry.delaysMs);
 const told=()=>h.sends().filter(s=>s.subject==="A deployer CLI call failed");
 assert.equal(told().length,1);assert.equal(told()[0].text,cliFailureNotice({reason,jobId:"rel_wait"},CODE_RUN).text);assert.ok(told()[0].text.includes(`Deployer CLI call failed: ${reason} (release rel_wait).`));assert.match(told()[0].text,SAFE_TEXT);
 for(const c of h.calls())for(const word of c.argv)assert.ok(!word.includes(HELD_SECRET)&&!word.includes("hub.invalid"));
 assert.equal(h.calls().filter(c=>c.argv[1]==="handler").length,3);assert.equal(existsSync(join(h.home,"rel_wait.json")),false);
 // A job that waits with no transport failure prints and posts nothing more.
 const quiet=codeHost(t,cwd);quiet.setJobs([verifiedJob("rel_quiet")]);
 assert.equal(await quiet.poll(null,async c=>({jobId:c.job.id,outcome:"waiting_inputs"})),"");assert.equal(quiet.sends().filter(s=>s.subject==="A deployer CLI call failed").length,0);
});

// A lasting transport wait (wi_f2e585b63f57b6f8). The fake hub fails the
// operations a file names with a transport line; every poll is a new daemon
// call, so each one is a restarted process over the same journal directory.
function waitHost(t){
 const cwd=mkdtempSync(join(tmpdir(),"release-transport-wait-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const sleep=cliRetry.sleep,slept=[];cliRetry.sleep=ms=>{slept.push(ms);};const clock=transportWait.now;let now=Date.parse("2026-10-13T18:00:00.000Z");transportWait.now=()=>now;t.after(()=>{cliRetry.sleep=sleep;transportWait.now=clock;});
 const faults=join(cwd,"faults.json");let failing=[];const fail=ops=>{failing=ops;writeFileSync(faults,JSON.stringify(ops));};fail([]);
 const h=codeHost(t,cwd,`const failing=JSON.parse(fs.readFileSync(${JSON.stringify(faults)},'utf8'));if(failing.includes(a[0]==='send'?'send':a[1])){fs.writeSync(2,'tt: Get "http://hub.invalid/'+${JSON.stringify(HELD_SECRET)}+'": dial tcp: connect: connection refused\\n');process.exit(1);}`);
 // The release reads the handler only while that read is failing: the fake hub has no handler of its own.
 const waits=async(c,adapter)=>{if(failing.includes("handler")){try{adapter.handler();}catch(error){assert.equal(error.transport,true);}}return {jobId:c.job.id,outcome:"waiting_inputs"};};
 const of=subject=>h.calls().filter(c=>c.argv[0]==="send").map(sendOf).filter(x=>x.subject===subject),file=join(h.home,"transport-wait.json");
 return {h,slept,fail,waits,poll:()=>h.poll(null,waits),advance:minutes=>{now+=minutes*60000;},at:()=>new Date(now).toISOString(),routine:()=>of("A deployer CLI call failed"),attention:()=>of(ATTENTION),file,wait:()=>JSON.parse(readFileSync(file,"utf8"))};
}
const ATTENTION="The deployer has not reached the hub for longer than its bound",WAITS_LINE="Release waits: a hub call failed in transport; the next poll retries.\n",ESCALATED_LINE="Transport wait passed its bound; attention notice posted.\n";
test("wait r3 a lasting transport wait posts one routine notice per wait and escalates once past the bound, across restarts, and resets after recovery",async t=>{
 const w=waitHost(t),reason="tt deployment handler connection refused, 3 tries",began=w.at();w.h.setJobs([verifiedJob("rel_wait")]);w.fail(["handler"]);
 assert.equal(transportWait.escalateMs,600000);
 // The first poll opens the wait: one routine notice, kept in the journal directory with the job and fixed words only.
 assert.equal(await w.poll(),WAITS_LINE);assert.equal(w.routine().length,1);assert.equal(w.attention().length,0);
 assert.deepEqual(w.wait(),{version:1,jobId:"rel_wait",since:began,cause:reason,polls:1,routine:true,escalated:false,latest:reason,lastAt:began});assert.equal(statSync(w.file).mode&0o777,0o600);
 assert.equal(w.routine()[0].text,cliFailureNotice({reason,jobId:"rel_wait"},CODE_RUN).text);assert.equal(w.routine()[0].requestId,cliFailureNotice({reason,jobId:"rel_wait",at:began},CODE_RUN).requestId);assert.deepEqual(w.routine()[0].refs,["release-job=rel_wait","wake=routine"]);
 // A restarted process inside the bound posts nothing more.
 w.advance(5);assert.equal(await w.poll(),WAITS_LINE);w.advance(4.9);assert.equal(await w.poll(),WAITS_LINE);assert.equal(w.routine().length,1);assert.equal(w.attention().length,0);assert.equal(w.wait().polls,3);assert.equal(w.wait().since,began);
 // Past the bound: exactly one attention notice, naming the job, the start and the cause.
 w.advance(0.1);assert.equal(await w.poll(),WAITS_LINE+ESCALATED_LINE);assert.equal(w.attention().length,1);const told=w.attention()[0];
 assert.equal(told.text,`Release rel_wait has waited on hub transport failures since ${began}, longer than the 10 minute bound. Cause: ${reason}. A job that is not yet published stays claimed and unrefused, and the deployer tries again at every poll. Check that the hub answers from the host of the deployer; see the transport wait step in docs/project-deployment.md.`);
 assert.match(told.text,SAFE_TEXT);assert.deepEqual(told.refs,["release-job=rel_wait","wake=attention"]);assert.equal(told.requestId,transportWaitNotice(w.wait()).requestId);assert.match(told.requestId,/^transport-wait-[a-f0-9]{16}$/);
 assert.equal(w.wait().escalated,"posted");assert.deepEqual(w.wait().due,{jobId:"rel_wait",cause:reason,minutes:10});
 // Later polls and restarts of the same wait repeat neither notice.
 w.advance(5);assert.equal(await w.poll(),WAITS_LINE);w.advance(30);assert.equal(await w.poll(),WAITS_LINE);assert.equal(w.routine().length,1);assert.equal(w.attention().length,1);
 for(const c of w.h.calls())for(const word of c.argv)assert.ok(!word.includes(HELD_SECRET)&&!word.includes("hub.invalid"));assert.ok(!readFileSync(w.file,"utf8").includes("hub.invalid"));
 // The first poll whose calls all succeed ends the wait.
 w.fail([]);assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),false);assert.equal(w.routine().length,1);assert.equal(w.attention().length,1);
 // A later failure is a new wait: its own routine notice, and its own escalation only after its own bound.
 w.advance(1);w.fail(["handler"]);const again=w.at();assert.equal(await w.poll(),WAITS_LINE);assert.equal(w.routine().length,2);assert.notEqual(w.routine()[1].requestId,w.routine()[0].requestId);assert.equal(w.wait().since,again);
 w.advance(9);await w.poll();assert.equal(w.attention().length,1);w.advance(1);await w.poll();assert.equal(w.attention().length,2);assert.notEqual(w.attention()[1].requestId,told.requestId);assert.ok(w.attention()[1].text.includes(`since ${again}`));
 // A failure of another job begins a new wait.
 w.h.setJobs([verifiedJob("rel_other")]);await w.poll();assert.equal(w.wait().jobId,"rel_other");assert.equal(w.wait().polls,1);assert.equal(w.wait().escalated,false);assert.equal(w.routine().length,3);
});
test("wait r3 notices skip retries during a transport wait, and an owed attention notice is posted when the hub answers again",async t=>{
 const w=waitHost(t),began=w.at();w.h.setJobs([verifiedJob("rel_wait")]);w.fail(["handler","send"]);
 const sends=()=>w.h.calls().filter(c=>c.argv[0]==="send").length;
 // The failing read spends its three tries; the routine notice is tried once, not three times, and stays owed.
 await w.poll();assert.deepEqual(w.slept,cliRetry.delaysMs);assert.equal(sends(),1);assert.equal(w.wait().routine,false);
 // Past the bound with the hub still failing: one try of each notice per poll, nothing marked posted.
 w.advance(11);assert.equal(await w.poll(),WAITS_LINE);assert.equal(w.slept.length,2*cliRetry.delaysMs.length);assert.equal(sends(),3);assert.equal(w.routine().length,2);assert.equal(w.attention().length,1);
 assert.equal(w.wait().routine,false);assert.equal(w.wait().escalated,false);assert.equal(w.wait().due.jobId,"rel_wait");
 // Sends still fail while reads succeed: the notice's own transport failure keeps the wait open.
 w.fail(["send"]);w.advance(1);assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),true);assert.equal(w.attention().length,2);assert.equal(w.wait().escalated,false);
 // The hub answers again: the owed attention notice is posted once, identical to the earlier tries, and the wait ends.
 w.fail([]);w.advance(1);assert.equal(await w.poll(),ESCALATED_LINE);assert.equal(existsSync(w.file),false);
 const tries=w.attention();assert.equal(tries.length,3);assert.equal(new Set(tries.map(x=>x.requestId+"\n"+x.text)).size,1);assert.ok(tries[0].text.includes(`since ${began}`));
 await w.poll();assert.equal(w.attention().length,3);
 // A wait that ends inside the bound owes nothing.
 w.fail(["handler"]);await w.poll();w.fail([]);w.advance(3);assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),false);assert.equal(w.attention().length,3);
});
test("wait r3 a poll that cannot read the job list at all continues the open wait and opens one of its own",async t=>{
 const w=waitHost(t);w.h.setJobs([verifiedJob("rel_wait")]);
 // No job is known: the wait and its attention notice name the polls of the deployer.
 w.fail(["list"]);const began=w.at();assert.match(await w.poll(),/^Deployment poll held \(tt deployment list connection refused, 3 tries\)/);
 assert.equal(w.wait().jobId,null);assert.equal(w.routine().length,1);assert.ok(!w.routine()[0].text.includes("release rel_"));
 w.advance(10);await w.poll();assert.equal(w.attention().length,1);assert.ok(w.attention()[0].text.startsWith(`The polls of the deployer have waited on hub transport failures since ${began}, longer than the 10 minute bound. Cause: tt deployment list connection refused, 3 tries.`));assert.match(w.attention()[0].text,SAFE_TEXT);assert.deepEqual(w.attention()[0].refs,["wake=attention"]);
 // A job's wait that a whole-poll failure follows stays that job's wait.
 const j=waitHost(t);j.h.setJobs([verifiedJob("rel_wait")]);j.fail(["handler"]);const first=j.at();await j.poll();j.advance(6);j.fail(["list"]);await j.poll();
 assert.equal(j.wait().jobId,"rel_wait");assert.equal(j.wait().since,first);assert.equal(j.wait().polls,2);assert.equal(j.wait().latest,"tt deployment list connection refused, 3 tries");
 j.advance(4);await j.poll();assert.equal(j.attention().length,1);assert.ok(j.attention()[0].text.startsWith("Release rel_wait has waited"));assert.ok(j.attention()[0].text.includes("Cause: tt deployment list connection refused, 3 tries."));
 // An unreadable wait file is no wait: nothing is escalated from it.
 writeFileSync(j.file,"{");j.fail([]);assert.equal(await j.poll(),"");assert.equal(j.attention().length,1);
});
