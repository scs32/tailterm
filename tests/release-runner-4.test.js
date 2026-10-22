import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,existsSync,statSync,readdirSync,rmSync,renameSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname} from "node:path";
import {runRelease,HostAdapter,publishedCode,codeDecision,heldRestartJob,codeRecord,codeNotice} from "../scripts/release-runner.mjs";
import {createHash} from "node:crypto";

// Release batching (wi_50e80d45647b342a slice one).
import {batchMismatch,batchPlanCovers} from "../scripts/release-runner.mjs";
import {planWithPreservation} from "../scripts/verify-matrix.mjs";

// File hashes stream through one fixed buffer (k1, k2, k4): a hub backup
// larger than 2 GiB is hashed without ever being held in memory.
import {openSync as openFile,ftruncateSync,closeSync as closeFile} from "node:fs";
import {randomBytes} from "node:crypto";
import {fileSHA256,FILE_HASH_CHUNK_BYTES,buildInputs as buildReleaseInputs} from "../scripts/release-inputs.mjs";
import {releaseReply,git,fixture,change,job,fake,config,hash,recorded,ignorePrerequisites,placePrerequisites,ended,idle,CODE_AGENT,CODE_RUN,SAFE_TEXT,codeRepo,codeHost,verifiedJob,codeGate,codeSends,heldOwn,heldJournal,HELD_SUBJECT,DRAINING_SUBJECT,batchConfig,batchJournal,matrixRepo,race,integratedPlan,covers,matrixFake,moveTip,hostPlan,checkIds,acceptDocs,stale,claimed,advancing,journalAt,refusable,nativeFixture,hubJob} from "./release-runner-fixtures.mjs";
test("held s1 each failed guard leaves the stale runner draining as before: no re-exec, no checkout move, the same notice, and its own job is still run",async t=>{
 const lockOf=cwd=>join(tmpdir(),"tailterm-release-locks",hash(cwd+"\0fixture")+".lock");
 const attempt=(h,run)=>{const dir=join(h.home,"rel_held-integrated-verification","d".repeat(40)+"-r1");mkdirSync(dir,{recursive:true});writeFileSync(join(dir,"run.json"),run);};
 const other={id:"rel_other",generation:3,commit:"d".repeat(40)};
 // [name, arrange(h,r,own) returning the job list or nothing, the jobs a poll still runs]
 const cases=[
  ["another blocked job",(h,r,own)=>[own,{...other,state:"blocked"}]],
  ["another merged job",(h,r,own)=>[own,{...other,state:"merged",agentId:"agt_other",runId:"run_other"}]],
  ["another claimed job",(h,r,own)=>[own,{...other,state:"claimed",agentId:"agt_other",runId:"run_other"}]],
  ["a second job of this claim",(h,r,own)=>[own,{...other,state:"claimed",agentId:CODE_AGENT,runId:CODE_RUN}]],
  ["the owned job is merged",(h,r,own)=>{const merged=heldOwn({state:"merged"});heldJournal(h,merged);return [merged];}],
  ["the holder is another agent",(h,r,own)=>{const j=heldOwn({agentId:"agt_other"});heldJournal(h,j);return [j];},[]],
  ["the holder is another run",(h,r,own)=>{const j=heldOwn({runId:"run_other"});heldJournal(h,j);return [j];},[]],
  ["the journal is missing",h=>{rmSync(join(h.home,"rel_held.json"));}],
  ["the journal is unreadable",h=>{writeFileSync(join(h.home,"rel_held.json"),"{not json");}],
  ["the journal is of another run",(h,r,own)=>{heldJournal(h,own,{runId:"run_other"});}],
  ["the journal is of another agent",(h,r,own)=>{heldJournal(h,own,{agentId:"agt_other"});}],
  ["the journal is of another commit",(h,r,own)=>{heldJournal(h,own,{commit:"d".repeat(40)});}],
  ["the journal is of another job",(h,r,own)=>{heldJournal(h,own,{jobId:"rel_other"});}],
  ["the journal is of another task",(h,r,own)=>{heldJournal(h,own,{taskId:"tsk_other"});}],
  ["the journal is of another pause generation",(h,r,own)=>{heldJournal(h,own,{pauseGeneration:4});}],
  ...["batching","waiting_matrix","waiting_inputs","pushing","receipt_pending","finishing","complete","refused","blocked","unknown_phase",undefined].map(phase=>[`the journal phase is ${phase}`,(h,r,own)=>{heldJournal(h,own,{phase});}]),
  ["a requeue lets the journal be archived and the job start again",(h,r,own)=>{const digest=hash(readFileSync(join(h.home,"rel_held.json")));return [heldOwn({reconciliations:[{disposition:"requeue",journalDigest:digest}]})];}],
  ["the journal has an effect",(h,r,own)=>{heldJournal(h,own,{effects:[{target:"hub"}]});}],
  ["the journal has no effects list",(h,r,own)=>{heldJournal(h,own,{effects:undefined});}],
  ["the journal is published",(h,r,own)=>{heldJournal(h,own,{published:true});}],
  ["the journal's published flag is not a boolean",(h,r,own)=>{heldJournal(h,own,{published:"no"});}],
  ["the hub shows the job published",()=>[heldOwn({published:true})]],
  ["the hub shows bound release inputs",()=>[heldOwn({inputsDigest:"f".repeat(64)})]],
  ["a matrix run of the job is started",h=>{attempt(h,JSON.stringify({state:"started"}));}],
  ["a matrix run of the job is starting",h=>{attempt(h,JSON.stringify({state:"starting"}));}],
  ["a matrix run record of the job is unreadable",h=>{attempt(h,"{not json");}],
  ["this process has a live matrix child",(h,r,own,t)=>{const children=new HostAdapter({},{}).matrixChildren,key="held-s1-fixture";children.set(key,{});return ()=>children.delete(key);}],
  ["a host release lock exists",(h,r)=>{const lock=lockOf(r.cwd);mkdirSync(dirname(lock),{recursive:true});writeFileSync(lock,JSON.stringify({jobId:"rel_held",agentId:CODE_AGENT,runId:CODE_RUN}));return ()=>rmSync(lock,{force:true});}],
 ];
 for(const [name,arrange,runs=["rel_held"]] of cases){
  const r=codeRepo(t),h=codeHost(t,r.cwd),own=heldOwn(),{gate,execs}=codeGate(r.codeA),released=[];r.at(r.a,r.b);heldJournal(h,own);
  const arranged=arrange(h,r,own),undo=typeof arranged==="function"?arranged:()=>{};t.after(undo);h.setJobs(Array.isArray(arranged)?arranged:[own]);
  let err;try{err=await h.poll(gate,async c=>{released.push(c.job.id);});err+=await h.poll(gate,async c=>{released.push(c.job.id);});}finally{undo();}
  // An unreadable journal of an active job holds the whole poll before the code gate, as it did.
  if(name==="the journal is unreadable"){assert.equal(execs.length,0);assert.equal(r.head(),r.a);assert.deepEqual(h.sends(),[]);assert.deepEqual(released,[]);assert.equal(existsSync(join(h.home,"runner-code.json")),false);assert.match(err,/^Deployment poll held \(/);continue;}
  assert.equal(execs.length,0,name);assert.equal(r.head(),r.a,name);assert.equal(h.claims().length,0,name);
  assert.equal(h.record().state,"draining",name);assert.equal(h.record().heldJob,undefined,name);
  assert.deepEqual(codeSends(h).map(s=>s.subject),[DRAINING_SUBJECT],name);
  assert.equal(codeSends(h)[0].requestId,`runner-code-${CODE_RUN}-${r.codeA.digest.slice(0,12)}-${r.codeB.digest.slice(0,12)}-draining`,name);
  assert.deepEqual(released,[...runs,...runs],name);
 }
 // An ended or set-aside matrix run is settled, so it does not stop the restart.
 for(const settle of [h=>attempt(h,JSON.stringify({state:"ended"})),h=>{attempt(h,"{}");const dir=join(h.home,"rel_held-integrated-verification","d".repeat(40)+"-r1");renameSync(join(dir,"run.json"),join(dir,"run.json.set-aside"));}]){
  const r=codeRepo(t),h=codeHost(t,r.cwd),own=heldOwn(),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);heldJournal(h,own);h.setJobs([own]);settle(h);
  await h.poll(gate);assert.equal(execs.length,1);assert.equal(h.record().heldJob,"rel_held");
 }
 // A stop signal: nothing moves and nothing is sent, as for the idle restart.
 const r=codeRepo(t),h=codeHost(t,r.cwd),own=heldOwn(),stop=new AbortController(),released=[];r.at(r.a,r.b);heldJournal(h,own);h.setJobs([own]);
 const {gate,execs}=codeGate(r.codeA,{published:cwd=>{setImmediate(()=>stop.abort());return publishedCode(cwd);}});
 assert.equal(await h.poll(gate,async c=>{released.push(c.job.id);},{signal:stop.signal}),"");
 assert.equal(execs.length,0);assert.equal(r.head(),r.a);assert.deepEqual(h.sends(),[]);assert.deepEqual(released,[]);assert.equal(existsSync(join(h.home,"runner-code.json")),false);
 // The predicate alone: null on any doubt about its inputs.
 const base={jobs:[own],details:new Map([[own.id,own]]),agent:CODE_AGENT,run:CODE_RUN,journalDirectory:h.home,lock:lockOf(r.cwd),matrixChildren:0};
 assert.equal(heldRestartJob(base),"rel_held");
 for(const over of [{jobs:[]},{details:new Map()},{agent:undefined},{run:""},{matrixChildren:1},{matrixChildren:undefined},{journalDirectory:join(h.home,"absent")},{details:new Map([[own.id,{...own,commit:"short"}]])},{details:new Map([[own.id,{...own,receipt:{}}]])},{details:null}])assert.equal(heldRestartJob({...base,...over}),null,JSON.stringify(Object.keys(over)));
 writeFileSync(join(h.home,"rel_held.json"),"{not json");assert.equal(heldRestartJob(base),null);
});
test("held s3 the idle restart is unchanged: same decision, record, notice text and request id, with no held job in any of them",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd),{gate,execs}=codeGate(r.codeA);r.at(r.a,r.b);h.setJobs([verifiedJob()]);
 await h.poll(gate);assert.equal(execs.length,1);assert.equal(h.record().state,"restart");assert.equal("heldJob" in h.record(),false);
 const l=r.codeA.digest.slice(0,12),c=r.codeB.digest.slice(0,12);
 assert.deepEqual(codeSends(h),[{subject:"Deployer is restarting itself onto the published scripts",requestId:`runner-code-${CODE_RUN}-${l}-${c}-restart`,
  text:`Deployer run ${CODE_RUN} loaded scripts ${l}; published tasks-hub ${r.b.slice(0,12)} has ${c}. Changed: release-runner.mjs. No release is active, so it restarts itself now with the same agent and run. A notice that it runs the published scripts follows; if none arrives the deployer did not come back. Action: run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md).`}]);
 // An idle runner never takes the held path, whatever it is told about a held job.
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:true,marker:null,held:"rel_held"}),{state:"restart"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeA,idle:false,held:"rel_held"}),{state:"current"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:false,marker:null,held:"rel_held"}),{state:"restart",heldJob:"rel_held"});
 for(const held of [null,undefined,"",true,0,{}])assert.deepEqual(codeDecision({loaded:r.codeA,published:r.codeB,idle:false,marker:null,held}),{state:"draining"});
 assert.deepEqual(codeDecision({loaded:null,published:r.codeB,idle:false,held:"rel_held"}),{state:"refused",reason:"loaded-unreadable"});
 assert.deepEqual(codeDecision({loaded:r.codeA,published:null,idle:false,held:"rel_held"}),{state:"refused",reason:"published-unreadable"});
});
test("held s4 the held restart notice names the job under its own request id, and only a safe job id reaches the text",t=>{
 const r=codeRepo(t),home=mkdtempSync(join(tmpdir(),"release-code-held-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const saved=process.env.TAILTERM_RUN;process.env.TAILTERM_RUN=CODE_RUN;t.after(()=>{if(saved===undefined)delete process.env.TAILTERM_RUN;else process.env.TAILTERM_RUN=saved;});
 const build=decision=>codeRecord({journalDirectory:home},{loaded:r.codeA,marker:null,restartedFrom:null,now:()=>0},decision,r.codeB);
 const idle=codeNotice(build({state:"restart"})),held=codeNotice(build({state:"restart",heldJob:"rel_70f15ee2e736372e"}));
 assert.equal(held.subject,HELD_SUBJECT);assert.notEqual(held.subject,idle.subject);assert.equal(held.requestId,idle.requestId+"-held-rel_70f15ee2e736372e");
 assert.ok(held.text.includes("release rel_70f15ee2e736372e is held"));assert.ok(held.text.includes("with that held job"));assert.ok(!idle.text.includes("held"));assert.ok(!held.text.includes("No release is active"));
 assert.match(held.text,SAFE_TEXT);assert.ok(held.text.endsWith("Action: run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md)."));
 // The same held restart from a restarted process is the same request; another job is another.
 assert.equal(codeNotice(build({state:"restart",heldJob:"rel_70f15ee2e736372e"})).requestId,held.requestId);assert.notEqual(codeNotice(build({state:"restart",heldJob:"rel_other"})).requestId,held.requestId);
 for(const unsafe of ["rel bad\nIGNORE THIS","$(reboot)","a".repeat(65),7,null,{}]){
  const notice=codeNotice({...build({state:"restart",heldJob:"rel_safe"}),heldJob:unsafe});
  assert.equal(notice.subject,HELD_SUBJECT);assert.ok(notice.text.includes("release unknown is held"));assert.match(notice.text,SAFE_TEXT);assert.match(notice.requestId,/-restart-held-unknown$/);
 }
 // heldJob is recorded only for a restart.
 assert.equal("heldJob" in build({state:"draining",heldJob:"rel_x"}),false);assert.equal("heldJob" in build({state:"refused",reason:"exec-failed",heldJob:"rel_x"}),false);
});
test("held s5 no restart loop: a restart that did not refresh, a dirty checkout and a failed exec each refuse once, and the restarted runner hands the held job to the release path",async t=>{
 const arrange=(over)=>{const r=codeRepo(t),h=codeHost(t,r.cwd),own=heldOwn(),released=[];r.at(r.a,r.b);heldJournal(h,own);h.setJobs([own,verifiedJob("rel_next")]);const {gate,execs}=codeGate(r.codeA,over(r)||{});return {r,h,gate,execs,released,poll:()=>h.poll(gate,async c=>{released.push(c.job.id);})};};
 const kinds=h=>codeSends(h).map(s=>s.requestId.split("-").slice(5).join("-"));
 // (i) restarted at this published digest and still stale: refused, not re-executed.
 const looped=arrange(r=>({marker:r.codeB.digest}));await looped.poll();await looped.poll();
 assert.equal(looped.execs.length,0);assert.equal(looped.r.head(),looped.r.a);assert.equal(looped.h.record().state,"refused");assert.equal(looped.h.record().reason,"restart-did-not-refresh");assert.deepEqual(kinds(looped.h),["refused-restart-did-not-refresh"]);
 assert.equal(looped.h.claims().length,0);assert.deepEqual(looped.released,["rel_held","rel_held"]);
 // (ii) the checkout cannot be prepared.
 const dirty=arrange(r=>{writeFileSync(join(r.cwd,"untracked.txt"),"x");});await dirty.poll();await dirty.poll();
 assert.equal(dirty.execs.length,0);assert.equal(dirty.r.head(),dirty.r.a);assert.equal(dirty.h.record().reason,"checkout-dirty");assert.deepEqual(kinds(dirty.h),["refused-checkout-dirty"]);assert.equal(dirty.h.claims().length,0);
 // (iii) the exec fails: tried once for this published digest.
 let tries=0;const failed=arrange(()=>({execve:()=>{tries++;throw new Error("SYNTHETIC exec failure");}}));await failed.poll();await failed.poll();await failed.poll();
 assert.equal(tries,1);assert.equal(failed.h.record().reason,"exec-failed");assert.deepEqual(kinds(failed.h),["restart-held-rel_held","refused-exec-failed"]);assert.equal(failed.h.claims().length,0);
 // (iv) after the restart: current code, the owned job goes to the release path with its journal untouched, and nothing else is claimed.
 const r=codeRepo(t),h=codeHost(t,r.cwd),own=heldOwn(),released=[];r.at(r.b,r.b);heldJournal(h,own);h.setJobs([own,verifiedJob("rel_next")]);
 const before=readFileSync(join(h.home,"rel_held.json"),"utf8"),{gate,execs}=codeGate(r.codeB,{marker:r.codeB.digest,restartedFrom:r.codeA.digest});
 await h.poll(gate,async c=>{released.push([c.job.id,c.job.state,c.job.agentId,c.job.runId,c.journalPath,c.code,readFileSync(c.journalPath,"utf8")]);});
 assert.deepEqual(released,[["rel_held","claimed",CODE_AGENT,CODE_RUN,join(h.home,"rel_held.json"),{loaded:r.codeB.digest,current:r.codeB.digest},before]]);
 assert.equal(execs.length,0);assert.equal(h.claims().length,0);assert.equal(h.record().state,"current");assert.equal(h.record().restartedFrom,r.codeA.digest);
 assert.deepEqual(codeSends(h).map(s=>s.subject),["Deployer now runs the published scripts"]);
});
test("select1 a docs-only job on a base older than the tip selects the docs checks only",async()=>{
 const m=matrixRepo(),docs=m.accept(1,"docs/a.md","a\n");ignorePrerequisites(m.f.cwd);placePrerequisites(m.f.cwd);
 const tip=moveTip(m);assert.notEqual(tip,m.base);assert.equal(docs.baseCommit,m.base);assert.deepEqual(checkIds(docs.plan),["npm-unit"]);
 const a=matrixFake(docs),c=batchConfig({f:m.f,lead:docs,members:[]},1);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");assert.ok(!a.calls.some(x=>x.startsWith("batch-")));
 assert.equal(a.verifies.length,1);const handed=a.verifies[0];
 assert.equal(handed.integratedCommit,r.commit);assert.notEqual(r.commit,docs.commit);
 assert.deepEqual(handed.selection,{base:tip,paths:["docs/a.md"]});assert.equal(batchJournal(c).expected,tip);
 assert.deepEqual(handed.plan,docs.plan,"the job's approved plan is handed over unchanged");
 const h=await hostPlan(m,handed);
 // The context names the tip beside the job's bound base; the plan keeps the base and gains no field.
 assert.equal(h.context.selectionBaseCommit,tip);assert.deepEqual(h.context.selectionPaths,["docs/a.md"]);assert.equal(h.context.baseCommit,m.base);assert.equal(h.context.commit,r.commit);
 assert.deepEqual(checkIds(h.plan),["npm-unit"]);assert.deepEqual(h.plan.checks,docs.plan.checks);assert.deepEqual(h.plan.changed,["docs/a.md"]);
 assert.equal(h.plan.baseCommit,m.base);assert.equal(h.plan.commit,r.commit);assert.deepEqual(Object.keys(h.plan).sort(),Object.keys(docs.plan).sort());
 assert.deepEqual(h.record.selection,{rule:"integration-tip",baseCommit:m.base,selectionBaseCommit:tip,selectionPaths:["docs/a.md"]});
 assert.match(h.stderr,new RegExp(`^Selected checks from the integration tip ${tip}\n`));
 // The run command is unchanged: plan path and attempt directory, no selection argument.
 assert.equal(h.started.length,1);assert.deepEqual(h.started[0].slice(0,3),["node","scripts/verify-matrix.mjs","run"]);assert.deepEqual(h.started[0].filter(x=>x.startsWith("--")),["--priority","--item","--host-wait-minutes"]);
 // Selected from the job's base, the same integrated commit runs the hub checks of the release the tip moved through.
 const before=integratedPlan(m,docs,r.commit);
 assert.deepEqual(checkIds(before),["go-race","go-test","go-vet","npm-unit"]);assert.deepEqual(before.changed,["docs/a.md","hub/internal/store/store.go"]);
});
test("select2 a Go job on the same moved tip still selects Go",async()=>{
 const m=matrixRepo(),code=m.accept(1,"hub/internal/api/api.go","package api\n\nvar Code = 1\n");ignorePrerequisites(m.f.cwd);placePrerequisites(m.f.cwd);
 const tip=moveTip(m,"docs/released.md","released\n");
 const a=matrixFake(code),c=batchConfig({f:m.f,lead:code,members:[]},1);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");
 const handed=a.verifies[0];assert.deepEqual(handed.selection,{base:tip,paths:["hub/internal/api/api.go"]});
 const h=await hostPlan(m,handed);
 assert.deepEqual(checkIds(h.plan),["go-race","go-test","go-vet"]);assert.deepEqual(h.plan.checks,code.plan.checks);assert.deepEqual(race(h.plan).slice(-1),["./internal/api"]);
 assert.deepEqual(checkIds(integratedPlan(m,code,r.commit)),["go-race","go-test","go-vet","npm-unit"]);
});
test("select3 a two-job docs batch on a base older than the tip selects the docs checks only",async()=>{
 const m=matrixRepo(),lead=m.accept(1,"docs/a.md","a\n"),member=acceptDocs(m,2,"docs/b.md","b\n");ignorePrerequisites(m.f.cwd);placePrerequisites(m.f.cwd);
 const tip=moveTip(m);assert.equal(lead.baseCommit,m.base);assert.equal(member.baseCommit,m.base);assert.equal(batchMismatch(lead,member),"");
 const a=matrixFake(lead),c=batchConfig({f:m.f,lead,members:[member]},2);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");
 assert.deepEqual(a.calls.filter(x=>x==="verify"||x.startsWith("batch-")),["batch-open","batch-add:tqe_m1","batch-add:tqe_m2","verify"]);
 const journal=batchJournal(c);assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_m1","rel_m2"]);assert.equal(journal.batch.base,tip);
 assert.equal(a.verifies.length,1);const handed=a.verifies[0];
 assert.equal(handed.integratedCommit,r.commit);assert.deepEqual(handed.selection,{base:tip,paths:["docs/a.md","docs/b.md"]});
 assert.deepEqual(git(m.f.cwd,"diff","--name-only",tip,r.commit).split("\n"),["docs/a.md","docs/b.md"]);
 const h=await hostPlan(m,handed);
 assert.equal(h.context.selectionBaseCommit,tip);assert.deepEqual(h.context.selectionPaths,["docs/a.md","docs/b.md"]);assert.equal(h.context.baseCommit,m.base);
 assert.deepEqual(checkIds(h.plan),["npm-unit"]);assert.deepEqual(h.plan.changed,["docs/a.md","docs/b.md"]);assert.equal(h.plan.baseCommit,m.base);
 assert.equal(batchPlanCovers(h.plan,handed.batchCovers),null);assert.equal(batchPlanCovers(h.plan,covers(lead,member)),null);assert.equal(h.started.length,1);
 assert.deepEqual(checkIds(integratedPlan(m,handed,r.commit)),["go-race","go-test","go-vet","npm-unit"]);
});
test("select4 a batch member's owned paths outside its diff stay selected and covered",async()=>{
 const m=matrixRepo(),lead=m.accept(1,"hub/internal/store/store.go","package store\n\nvar Lead = 1\n"),member=m.accept(2,"hub/internal/api/api.go","package api\n\nvar Member = 1\n");
 // The member also owns a package its commit does not change, so its accepted go-race names it.
 member.plan=planWithPreservation({operationKey:"fixture-2",repository:"fixture",baseCommit:m.base,commit:member.commit,owned:["hub/cmd/tt/main.go","hub/internal/api/api.go"],verifierAgentId:"agt_verifier",verifierRunId:"run_verifier",approvedMatrixDigest:m.approved,matrixApprovalMessageSeq:9},m.f.cwd).plan;
 assert.deepEqual(member.plan.changed,["hub/internal/api/api.go"]);assert.deepEqual(race(member.plan).slice(-2),["./cmd/tt","./internal/api"]);
 ignorePrerequisites(m.f.cwd);placePrerequisites(m.f.cwd);const tip=moveTip(m,"docs/released.md","released\n");
 const a=matrixFake(lead),c=batchConfig({f:m.f,lead,members:[member]},2);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");assert.equal(a.verifies.length,1);
 const handed=a.verifies[0];
 assert.deepEqual(handed.selection,{base:tip,paths:["hub/cmd/tt/main.go","hub/internal/api/api.go","hub/internal/store/store.go"]});
 const h=await hostPlan(m,handed);
 assert.deepEqual(checkIds(h.plan),["go-race","go-test","go-vet"]);assert.deepEqual(race(h.plan).slice(-3),["./cmd/tt","./internal/api","./internal/store"]);
 assert.deepEqual(h.plan.changed,["hub/cmd/tt/main.go","hub/internal/api/api.go","hub/internal/store/store.go"]);
 assert.equal(batchPlanCovers(h.plan,handed.batchCovers),null);assert.equal(batchPlanCovers(h.plan,covers(lead,member)),null);assert.equal(h.started.length,1);
});
test("select5 a job or batch whose changed paths cannot be read hands no selection and selects from the job's base",async()=>{
 const m=matrixRepo(),docs=m.accept(1,"docs/a.md","a\n"),member=acceptDocs(m,2,"docs/b.md","b\n");ignorePrerequisites(m.f.cwd);placePrerequisites(m.f.cwd);
 moveTip(m);
 for(const plan of [{...docs.plan,changed:undefined},{...docs.plan,changed:"docs/a.md"},{...docs.plan,owned:undefined}]){
  git(m.f.cwd,"checkout","--quiet","--detach","tasks-hub");const before=git(m.f.cwd,"rev-parse","refs/heads/tasks-hub");
  const job={...docs,plan},a=matrixFake(job),c=batchConfig({f:m.f,lead:job,members:[]},1);
  const r=await runRelease(c,a);assert.equal(r.outcome,"released");
  const handed=a.verifies[0];assert.ok(!("selection" in handed));
  if(plan.owned){
   const h=await hostPlan(m,handed);
   assert.ok(!("selectionBaseCommit" in h.context)&&!("selectionPaths" in h.context));assert.equal(h.context.baseCommit,m.base);
   assert.deepEqual(checkIds(h.plan),["go-race","go-test","go-vet","npm-unit"]);assert.equal(h.record.selection,undefined);assert.doesNotMatch(h.stderr,/Selected checks from/);
  }
  // The next case integrates the same job again from the tip as it was.
  git(m.f.cwd,"checkout","--quiet","--detach",before);git(m.f.cwd,"update-ref","refs/heads/tasks-hub",before);
 }
 // A batch with a member whose plan names no changed paths, or whose record is gone, falls back whole.
 for(const members of [[{...member,plan:{...member.plan,changed:undefined}}],null]){
  const before=git(m.f.cwd,"rev-parse","refs/heads/tasks-hub");
  const a=matrixFake(docs),c=batchConfig({f:m.f,lead:docs,members:members||[member]},2);
  if(!members){const add=a.batch;a.batch=async(...x)=>{const out=await add(...x);if(x[0]==="batch-add"&&x[1].entryId==="tqe_m2")c.batch.candidates=[];return out;};}
  const r=await runRelease(c,a);assert.equal(r.outcome,"released");
  assert.deepEqual(batchJournal(c).batch.jobs.map(j=>j.jobId),["rel_m1","rel_m2"]);
  const handed=a.verifies[0];assert.ok(!("selection" in handed),members?"unreadable member plan":"missing member record");
  git(m.f.cwd,"checkout","--quiet","--detach",before);git(m.f.cwd,"update-ref","refs/heads/tasks-hub",before);
 }
});
test("g1 a generation advanced between claim and fence is adopted once and the release publishes",async()=>{
 const f=fixture(),commit=change(f,"client/a.js","a"),a=advancing([2]),c=config(f,claimed(f,commit));
 const receipt=await runRelease(c,a);
 assert.equal(receipt.outcome,"released");assert.deepEqual(a.calls,["merged","deploy:tailos","finish"]);assert.ok(!a.calls.includes("refuse"));
 assert.equal(a.rereads,1);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),commit);
 const journal=journalAt(c);assert.equal(journal.phase,"complete");assert.equal(journal.generationAdoptions,1);assert.equal(journal.refusalReason,undefined);
});
test("g1 a claim that is no longer this run's is a lost fence with no effects, and other check failures are not re-read",async()=>{
 const f=fixture(),commit=change(f,"client/a.js","a"),a=advancing([2],false),c=config(f,claimed(f,commit));
 a.refuse=async()=>{a.calls.push("refuse");throw stale();};
 await assert.rejects(runRelease(c,a));
 assert.deepEqual(a.calls,["refuse"],"one refuse call, which the hub answers for the claim it has");assert.equal(a.rereads,2,"one re-read for the check and one for the refuse");
 const journal=journalAt(c);assert.equal(journal.phase,"refusing");assert.equal(journal.refusalReason,"release fence lost");assert.deepEqual(journal.effects,[]);assert.notEqual(journal.published,true);assert.equal(journal.generationAdoptions,undefined);
 assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);
 const g=fixture(),b=fake(),d=config(g,claimed(g,change(g,"client/a.js","a")));b.rereads=0;b.fence=async()=>false;b.adoptGeneration=async()=>{b.rereads++;return true;};
 await assert.rejects(runRelease(d,b),/release fence lost/);assert.equal(b.rereads,0);assert.deepEqual(b.calls,[]);assert.ok(!existsSync(d.journalPath));
});
test("g1 no loop: a check still answered generation changed after the one re-read is a lost fence",async()=>{
 const f=fixture(),a=advancing([1,2,3,4,5,6]),c=config(f,claimed(f,change(f,"client/a.js","a")));
 await assert.rejects(runRelease(c,a),/release fence lost/);
 assert.equal(a.fences,2);assert.equal(a.rereads,1);assert.deepEqual(a.calls,[]);assert.ok(!existsSync(c.journalPath));assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);
});
test("g2 a refuse answered generation changed is sent once more at the re-read generation",async()=>{
 const f=fixture(),a=advancing([]),c=config(f,refusable(f));let refuses=0,escalation;
 a.refuse=async()=>{a.calls.push("refuse");if(++refuses===1)throw stale();};a.escalate=async d=>{escalation=d;a.calls.push("escalate");};
 await assert.rejects(runRelease(c,a),/Release refused before publication/);
 assert.deepEqual(a.calls,["refuse","refuse","escalate"]);assert.equal(a.rereads,1);assert.deepEqual(escalation,{jobId:"rel_fixture",outcome:"refused",reason:"Release binding mismatch"});
 const journal=journalAt(c);assert.equal(journal.phase,"refused");assert.equal(journal.refuseFailure,undefined);assert.equal(journal.generationAdoptions,1);assert.equal(journal.refusalReason,"Release binding mismatch");
});
test("g2 a journal left refusing by a failed refuse is resumed by the next poll and completes the refusal",async()=>{
 const f=fixture(),a=advancing([]),j=refusable(f),c=config(f,j);
 a.refuse=async()=>{a.calls.push("refuse");throw stale();};
 await assert.rejects(runRelease(c,a));
 assert.deepEqual(a.calls,["refuse","refuse"],"no loop: one retry");assert.equal(a.rereads,1);
 const stuck=journalAt(c);assert.equal(stuck.phase,"refusing");assert.equal(stuck.refuseFailure,"unclassified");assert.equal(stuck.refusalReason,"Release binding mismatch");
 // The next poll: the same claim, read again at the hub's generation.
 const head=git(f.cwd,"rev-parse","HEAD"),b=advancing([]);let escalation;b.escalate=async d=>{escalation=d;b.calls.push("escalate");};
 b.verifyIntegrated=async()=>{throw new Error("a resumed refusal starts no matrix run");};
 await assert.rejects(runRelease(c,b),/Release refused before publication/);
 assert.deepEqual(b.calls,["refuse","escalate"]);assert.equal(b.fences,1,"only the entry check");assert.deepEqual(escalation,{jobId:"rel_fixture",outcome:"refused",reason:"Release binding mismatch"});
 const done=journalAt(c);assert.equal(done.phase,"refused");assert.equal(done.refuseFailure,undefined);assert.equal(done.refuseRecovered,"unclassified");assert.equal(done.refusalReason,"Release binding mismatch");
 assert.equal(git(f.cwd,"rev-parse","HEAD"),head);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),f.base);
 // A refuse that fails again leaves it refusing for the poll after.
 writeFileSync(c.journalPath,JSON.stringify(stuck));const again=advancing([]);again.refuse=async()=>{again.calls.push("refuse");throw new Error("hub unreachable");};
 await assert.rejects(runRelease(c,again),/hub unreachable/);assert.deepEqual(again.calls,["refuse"]);assert.equal(again.rereads,0);assert.equal(journalAt(c).phase,"refusing");
});
test("g3 a refusing journal of another run, with effects, published or with no failed refuse is never resumed",async()=>{
 const f=fixture(),j=refusable(f),c=config(f,j);
 const stuck={version:1,jobId:j.id,commit:j.commit,agentId:j.agentId,runId:j.runId,phase:"refusing",refusalReason:"release fence lost",refuseFailure:"tt deployment refuse exit 1",effects:[]};
 for(const [name,change] of [["another run",{runId:"run_other"}],["another agent",{agentId:"agt_other"}],["an effect",{effects:[{target:"tailos",release:"r",state:"attempting"}]}],["published",{published:true}],["no failed refuse",{refuseFailure:undefined}],["another pause generation",{pauseGeneration:7}]]){
  writeFileSync(c.journalPath,JSON.stringify({...stuck,...change}));const a=advancing([]);
  await assert.rejects(runRelease(c,a),/Ambiguous journal requires handler reconciliation/,name);assert.deepEqual(a.calls,[],name);assert.equal(a.rereads,0,name);
  assert.deepEqual(journalAt(c),JSON.parse(JSON.stringify({...stuck,...change})),name+": journal untouched");
 }
 // A resumed refusal whose entry check finds the claim gone refuses nothing.
 writeFileSync(c.journalPath,JSON.stringify(stuck));const lost=advancing([1],false);
 await assert.rejects(runRelease(c,lost),/release fence lost/);assert.deepEqual(lost.calls,[]);assert.equal(journalAt(c).phase,"refusing");
});
test("g3 once the release is published or an effect is journaled a generation change is never adopted",async()=>{
 // Fence calls of a fast-forward release: entry, start, publish, prepare, deploy.
 for(const [at,published,effects] of [[4,true,0],[5,true,1]]){
  const f=fixture(),a=advancing([at]),c=config(f,claimed(f,change(f,"client/a.js","a")));
  await assert.rejects(runRelease(c,a));
  assert.equal(a.rereads,0,`fence call ${at}`);assert.ok(!a.calls.includes("refuse"));
  const journal=journalAt(c);assert.equal(journal.published,published);assert.equal(journal.effects.length,effects);assert.equal(journal.generationAdoptions,undefined);assert.equal(journal.phase,"blocked");
 }
});
test("g1 the host adapter names the hub's generation changed, re-reads once and adopts only its own exact claim",async()=>{
 const n=nativeFixture(hubJob(3,{integratedVerification:{commit:"b".repeat(40)}})),adapter=new HostAdapter(n.config,hubJob(2));
 assert.equal(await adapter.fence(),"generation-changed");assert.equal(adapter.job.generation,2,"a failed check adopts nothing");
 assert.equal(await adapter.adoptGeneration(),true);assert.equal(adapter.job.generation,3);assert.equal(await adapter.fence(),true);
 assert.deepEqual(n.calls(),["deployment check 2","deployment get","deployment check 3"]);
 assert.equal(await adapter.adoptGeneration(),false,"a generation that did not advance is not adopted");
 // Another run, another agent, another pause generation, a changed pin or a job no longer claimed: never adopted.
 for(const [name,other] of [["run",{runId:"run_other"}],["agent",{agentId:"agt_other"}],["pause",{pauseGeneration:2}],["commit",{commit:"d".repeat(40)}],["verified again",{state:"verified",agentId:undefined,runId:undefined}],["refused",{state:"refused"}],["merged",{state:"merged"}]]){
  const m=nativeFixture(hubJob(3,other)),stale=new HostAdapter(m.config,hubJob(2));
  assert.equal(await stale.fence(),"generation-changed",name);assert.equal(await stale.adoptGeneration(),false,name);assert.equal(stale.job.generation,2,name);assert.equal(stale.job.runId,"run_fixture",name);
 }
 // Any other check failure, another conflict included, is a plain lost fence.
 const p=nativeFixture(hubJob(3),{conflict:"conflict: release: project generation changed"});assert.equal(await new HostAdapter(p.config,hubJob(2)).fence(),false);
 const q=nativeFixture(hubJob(3),{conflict:"conflict: release: job rel_fixture is claimed by another run"});assert.equal(await new HostAdapter(q.config,hubJob(2)).fence(),false);
});
test("g2 g3 through the host adapter: the stuck check and refuse of the incident end in a refusal at the re-read generation, and never for another run",async()=>{
 const f=fixture(),commit=change(f,"client/a.js","a"),mine={baseCommit:f.base,commit,plan:{commit:f.base}};
 // The handler's import moved the job from 2 to 3 while this run still held 2.
 const n=nativeFixture(hubJob(3,mine)),adapter=new HostAdapter({...n.config,cwd:f.cwd},hubJob(2,mine));let escalation;adapter.escalate=async d=>{escalation=d;};
 const c={...config(f,adapter.job),journalPath:join(n.home,"rel_fixture.json")};
 await assert.rejects(runRelease(c,adapter),/Release refused before publication/);
 assert.deepEqual(n.calls(),["deployment check 2","deployment get","deployment check 3","deployment check 3","deployment refuse 3"]);
 assert.equal(adapter.job.state,"refused");assert.equal(escalation.reason,"Release binding mismatch");assert.equal(journalAt(c).generationAdoptions,1);
 // The advance lands between the last check and the refuse: one re-read, one more refuse.
 const h=fixture(),late={baseCommit:h.base,commit:change(h,"client/a.js","a"),plan:{commit:h.base}},r=nativeFixture(hubJob(2,late),{advanceOn:"refuse"}),retried=new HostAdapter({...r.config,cwd:h.cwd},hubJob(2,late));retried.escalate=async()=>{};
 const e={...config(h,retried.job),journalPath:join(r.home,"rel_fixture.json")};
 await assert.rejects(runRelease(e,retried),/Release refused before publication/);
 assert.deepEqual(r.calls(),["deployment check 2","deployment check 2","deployment refuse 2","deployment get","deployment refuse 3"]);assert.equal(retried.job.state,"refused");assert.equal(journalAt(e).phase,"refused");
 // The same advance, but the claim went to another run: no adoption, and the refuse stays at the stale generation, which the hub rejects.
 const g=fixture(),other={baseCommit:g.base,commit:change(g,"client/a.js","a")},m=nativeFixture(hubJob(3,{...other,runId:"run_other"})),lost=new HostAdapter({...m.config,cwd:g.cwd},hubJob(2,other));
 const d={...config(g,lost.job),journalPath:join(m.home,"rel_fixture.json")};
 await assert.rejects(runRelease(d,lost),/release fence lost/);
 assert.deepEqual(m.calls(),["deployment check 2","deployment get"]);assert.equal(lost.job.generation,2);assert.ok(!existsSync(d.journalPath));assert.equal(git(g.cwd,"rev-parse","tasks-hub"),g.base);
});
test("the streamed file hash equals a whole-file hash at every chunk boundary",t=>{
 const home=mkdtempSync(join(tmpdir(),"release-file-hash-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 assert.equal(FILE_HASH_CHUNK_BYTES,1024*1024);
 // Empty, under one chunk, either side of a chunk, an exact multiple, and several chunks with a remainder.
 for(const size of [0,5,FILE_HASH_CHUNK_BYTES-1,FILE_HASH_CHUNK_BYTES,FILE_HASH_CHUNK_BYTES+1,2*FILE_HASH_CHUNK_BYTES,3*FILE_HASH_CHUNK_BYTES+7]){
  const path=join(home,"f"+size);writeFileSync(path,randomBytes(size));
  assert.equal(fileSHA256(path),createHash("sha256").update(readFileSync(path)).digest("hex"),`${size} bytes`);
 }
 assert.throws(()=>fileSHA256(join(home,"absent")),{code:"ENOENT"});
});
test("a sparse backup over 2 GiB hashes through the input producer and the rehearsal check in bounded memory",{timeout:600000},async t=>{
 // The size of the hub backup that the whole-file read refused.
 const SIZE=2209042432,GIB2=2*1024**3;assert.ok(SIZE>GIB2);
 // The reference: that many zero bytes streamed through the hash, with no file involved.
 const zeros=Buffer.alloc(1024*1024),reference=createHash("sha256");
 for(let left=SIZE;left>0;left-=zeros.length)reference.update(left<zeros.length?zeros.subarray(0,left):zeros);
 const expected=reference.digest("hex");
 const f=fixture(),home=mkdtempSync(join(tmpdir(),"release-large-backup-")),template=join(home,"plan-template.json");
 t.after(()=>{for(const d of [home,f.cwd,f.origin])rmSync(d,{recursive:true,force:true});});
 mkdirSync(join(f.cwd,"hub/internal/store"),{recursive:true});const commit=change(f,"hub/internal/store/x.go","schema");
 writeFileSync(template,JSON.stringify({version:1,deployment:{stateDirectory:"/mnt/deepfreeze/tailterm-hub/state",binaryDestination:"stale",bridgeBinaryDestination:"stale"}}));
 const config={version:1,cwd:f.cwd,journalDirectory:home,baselines:Object.fromEntries(["hub","bridge","mini","tailos"].map(x=>[x,f.base])),inputs:{planTemplate:template}};
 const j={...job(f,commit),id:"rel_0123abcd",taskId:"tsk_fixture",generation:2,agentId:"agt_fixture",runId:"run_fixture",pauseGeneration:0,integratedCommit:commit};
 const sparse=path=>{const fd=openFile(path,"wx",0o600);try{ftruncateSync(fd,SIZE);}finally{closeFile(fd);}};
 const live={commit:"1".repeat(40),artifactSHA256:"2".repeat(64),integrity:true,release:"rel_prev-111111111111-hub"};
 const deps={configPath:join(home,"deploy.json"),tt:argv=>JSON.stringify(releaseReply([j],argv)),git:argv=>git(f.cwd,...argv),probe:async()=>live,
  preflight:(plan,receipt)=>writeFileSync(receipt,JSON.stringify({status:"success",backupDestination:JSON.parse(readFileSync(plan,"utf8")).backupDestination,sha256:expected})),
  copyBackup:(remote,local)=>sparse(local)};
 // Every buffer asked for while the two scripts hash the backup, and the peak resident size around them.
 const asked=[],real={};for(const name of ["alloc","allocUnsafe","allocUnsafeSlow"]){real[name]=Buffer[name];Buffer[name]=function(size,...rest){asked.push(size);return real[name].call(Buffer,size,...rest);};}
 const peakBefore=process.resourceUsage().maxRSS;let out,hub;
 try{
  // The release-inputs path: the backup copy is hashed against the preflight receipt.
  out=await buildReleaseInputs(config,j.id,{deps});hub=JSON.parse(readFileSync(out.manifest,"utf8")).targets.hub;
  assert.equal(hub.backupSHA256,expected);assert.equal(statSync(hub.backupCopy).size,SIZE);
  // The runner's rehearsal check on that manifest entry: the hash matches, so the next refusal is the stopped runtime.
  const adapter=new HostAdapter({cwd:home,journalDirectory:home},{id:j.id});adapter.command=()=>assert.fail("host command");adapter.container=()=>({status:1,stdout:"",stderr:""});
  const artifact={backupCopy:hub.backupCopy,backupSHA256:hub.backupSHA256,migrationBinary:join(home,"migration"),planPath:hub.planPath,release:hub.release};
  await assert.rejects(adapter.rehearse(artifact),/^Error: Rehearsal container runtime not running$/);
  // One changed digit is still the unchanged refusal, so the check read the file.
  await assert.rejects(adapter.rehearse({...artifact,backupSHA256:expected.replace(/.$/,c=>c==="0"?"1":"0")}),/^Error: Imported backup hash mismatch$/);
 }finally{Object.assign(Buffer,real);}
 const grewKiB=process.resourceUsage().maxRSS-peakBefore;
 assert.ok(asked.filter(size=>size===FILE_HASH_CHUNK_BYTES).length>=3,"each hash asks for one chunk buffer");
 assert.ok(Math.max(...asked)<=FILE_HASH_CHUNK_BYTES,`largest buffer asked for was ${Math.max(...asked)} bytes`);
 assert.ok(grewKiB<256*1024,`peak resident memory grew ${grewKiB} KiB`);
 // The sparse copy took no real space, was not copied, and left no rehearsal state behind.
 assert.ok(statSync(hub.backupCopy).blocks*512<SIZE/64,"the backup copy is sparse");
 assert.deepEqual(readdirSync(home).filter(n=>n.includes(".rehearsal-")),[]);
 assert.deepEqual(out.targets,["hub","bridge","mini"]);
});

// Clean-stop unclaim (wi_27745813129a396b, order #38456). A runner stopped
// with SIGTERM or SIGINT gives back its own claimed job when it has no
// effects; anything else stays held for handler reconciliation.
import {setAsideJournal,unclaimableJob} from "../scripts/release-runner.mjs";
// The fake hub's unclaim, as the hub answers it: verified, no run, one more
// unclaimed record, saved in place. refuse makes it a refusal instead.
const unclaimSave=`const path=require('path').join(require('path').dirname(process.argv[1]),'jobs.json'),{agentId,runId,integratedCommit,...rest}=job;
const next={...rest,state:'verified',generation:+flag('--generation')+1,reconciliations:[...(job.reconciliations||[]),{disposition:'unclaimed',jobId:job.id,agentId,runId,journalState:'no_effects',noPublication:true}]};
jobs.splice(jobs.indexOf(job),1,next);fs.writeFileSync(path,JSON.stringify(jobs));`;
const unclaimHook=(refuse=false)=>`if(a[1]==='unclaim'){${refuse?"process.stderr.write('tt: conflict: only a claimed job with no effects can be unclaimed\\n');process.exit(1);":""}
${unclaimSave}console.log(JSON.stringify(next));process.exit(0);}`;
const waitingJournal=(h,job,over={})=>heldJournal(h,job,{phase:"waiting_inputs",refusalReason:undefined,refuseFailure:undefined,integrated:"d".repeat(40),...over});
const stopAttempt=(h,run)=>{const dir=join(h.home,"rel_held-integrated-verification","d".repeat(40)+"-r0");mkdirSync(dir,{recursive:true});writeFileSync(join(dir,"run.json"),run);};
const unclaims=h=>h.calls().filter(c=>c.argv[1]==="unclaim").map(c=>c.argv);
const ledger=h=>JSON.parse(readFileSync(join(h.home,"jobs.json"),"utf8"));
// One poll whose stop signal is already set: the loop is skipped and only the stop runs.
const stopped=h=>{const stop=new AbortController();stop.abort();return h.poll(undefined,async()=>assert.fail("a stopping runner releases nothing"),{once:true,signal:stop.signal});};
const HELD_LINE="Stopped while holding release rel_held; handler reconciliation required.\n";
test("stop1 a clean stop in waiting_inputs with no effects returns the job to verified, archives its journal and the next poll claims it",async t=>{
 for(const [phase,arrange] of [["waiting_inputs",()=>{}],["waiting_matrix",h=>stopAttempt(h,JSON.stringify({state:"ended"}))]]){
  const r=codeRepo(t),h=codeHost(t,r.cwd,unclaimHook()),own=heldOwn();waitingJournal(h,own,{phase});arrange(h);h.setJobs([own,verifiedJob("rel_next")]);
  const before=readFileSync(join(h.home,"rel_held.json"),"utf8");
  assert.equal(await stopped(h),"Release rel_held returned to verified at stop; journal archived.\n",phase);
  assert.deepEqual(unclaims(h),[["deployment","unclaim","--job","rel_held","--generation","2","--request-id","rel_held-unclaim-2"]],phase);
  assert.equal(existsSync(join(h.home,"rel_held.json")),false,phase);assert.equal(readFileSync(join(h.home,"rel_held.json.unclaimed-g3"),"utf8"),before,phase);
  const [freed,next]=ledger(h);
  assert.deepEqual([freed.id,freed.state,freed.generation,freed.agentId,freed.runId,next.id],["rel_held","verified",3,undefined,undefined,"rel_next"],phase);
  assert.deepEqual(freed.reconciliations.map(x=>[x.disposition,x.agentId,x.runId]),[["unclaimed",CODE_AGENT,CODE_RUN]],phase);
  assert.equal(h.claims().length,0,phase);assert.deepEqual(h.sends(),[],phase);
  // The next run claims it first, as a fresh attempt: one more record than before.
  const released=[];await h.poll(undefined,async c=>{released.push([c.job.id,c.job.state,c.job.reconciliations.length,existsSync(c.journalPath)]);});
  assert.deepEqual(h.claims().map(c=>c.argv.slice(1,7)),[["claim","--job","rel_held","--generation","3","--request-id"]],phase);
  assert.deepEqual(released,[["rel_held","claimed",1,false]],phase);
 }
});
test("stop2 a stop after any effect, or on any doubt, keeps the claim and journal for handler reconciliation",async t=>{
 const lockOf=cwd=>join(tmpdir(),"tailterm-release-locks",hash(cwd+"\0fixture")+".lock");
 const other={id:"rel_other",generation:3,commit:"d".repeat(40)};
 // [name, arrange(h,r,own) returning the job list or an undo, the stderr, the unclaim calls]
 const cases=[
  ["the journal has an effect",(h,r,own)=>{waitingJournal(h,own,{effects:[{target:"hub"}]});}],
  ["the journal has no effects list",(h,r,own)=>{waitingJournal(h,own,{effects:undefined});}],
  ["the journal is published",(h,r,own)=>{waitingJournal(h,own,{published:true});}],
  ["the journal's published flag is not a boolean",(h,r,own)=>{waitingJournal(h,own,{published:"no"});}],
  ["the merged call was sent",(h,r,own)=>{waitingJournal(h,own,{mergedSent:true});}],
  ["the journal is of a batch",(h,r,own)=>{waitingJournal(h,own,{batch:{jobs:[]}});}],
  ...["prepared","integrated","batching","refusing","pushing","receipt_pending","finishing","complete","blocked",undefined].map(phase=>[`the journal phase is ${phase}`,(h,r,own)=>{waitingJournal(h,own,{phase});}]),
  ["the journal is missing",h=>{rmSync(join(h.home,"rel_held.json"));}],
  ["the journal is unreadable",h=>{writeFileSync(join(h.home,"rel_held.json"),"{not json");}],
  ["the journal is of another job",(h,r,own)=>{waitingJournal(h,own,{jobId:"rel_other"});}],
  ["the journal is of another commit",(h,r,own)=>{waitingJournal(h,own,{commit:"d".repeat(40)});}],
  ["the journal is of another agent",(h,r,own)=>{waitingJournal(h,own,{agentId:"agt_other"});}],
  ["the journal is of another run",(h,r,own)=>{waitingJournal(h,own,{runId:"run_other"});}],
  ["the journal is of another task",(h,r,own)=>{waitingJournal(h,own,{taskId:"tsk_other"});}],
  ["the journal is of another pause generation",(h,r,own)=>{waitingJournal(h,own,{pauseGeneration:4});}],
  ["the hub shows the job published",()=>[heldOwn({published:true})]],
  ["the hub shows a receipt",()=>[heldOwn({receipt:{commit:"c".repeat(40),outcome:"blocked",targets:[]}})]],
  ["the hub shows bound release inputs",()=>[heldOwn({inputsCommit:"c".repeat(40),inputsDigest:"f".repeat(64)})]],
  ["the job is merged",()=>[heldOwn({state:"merged",published:true,integratedCommit:"c".repeat(40)})]],
  ["the job is blocked",()=>[heldOwn({state:"blocked"})]],
  ["another job is claimed",(h,r,own)=>[own,{...other,state:"claimed",agentId:"agt_other",runId:"run_other"}]],
  ["a matrix run of the job is started",h=>{stopAttempt(h,JSON.stringify({state:"started"}));}],
  ["a matrix run of the job is starting",h=>{stopAttempt(h,JSON.stringify({state:"starting"}));}],
  ["a matrix run record of the job is unreadable",h=>{stopAttempt(h,"{not json");}],
  ["this process has a live matrix child",()=>{const children=new HostAdapter({},{}).matrixChildren,key="stop2-fixture";children.set(key,{});return ()=>children.delete(key);}],
  ["a host release lock exists",(h,r)=>{const lock=lockOf(r.cwd);mkdirSync(dirname(lock),{recursive:true});writeFileSync(lock,JSON.stringify({jobId:"rel_held",agentId:CODE_AGENT,runId:CODE_RUN}));return ()=>rmSync(lock,{force:true});}],
  ["the hub refuses the unclaim",()=>{},HELD_LINE,1,true],
  ["the job is claimed by another run",()=>[heldOwn({runId:"run_other"})],""],
 ];
 for(const [name,arrange,err=HELD_LINE,calls=0,refuse=false] of cases){
  const r=codeRepo(t),h=codeHost(t,r.cwd,unclaimHook(refuse)),own=heldOwn();waitingJournal(h,own);
  const arranged=arrange(h,r,own),undo=typeof arranged==="function"?arranged:()=>{};t.after(undo);h.setJobs(Array.isArray(arranged)?arranged:[own]);
  const journal=existsSync(join(h.home,"rel_held.json"))?readFileSync(join(h.home,"rel_held.json"),"utf8"):null,jobs=ledger(h);
  let said;try{said=await stopped(h);}finally{undo();}
  assert.equal(said,err,name);assert.equal(unclaims(h).length,calls,name);assert.deepEqual(ledger(h),jobs,name);
  assert.equal(existsSync(join(h.home,"rel_held.json"))?readFileSync(join(h.home,"rel_held.json"),"utf8"):null,journal,name);
  assert.deepEqual(readdirSync(h.home).filter(n=>n.includes(".unclaimed-")),[],name);assert.equal(h.claims().length,0,name);
 }
 // A ledger that cannot be read says so, and nothing is unclaimed.
 const r=codeRepo(t),h=codeHost(t,r.cwd,"if(a[1]==='list')process.exit(3);"),own=heldOwn();waitingJournal(h,own);h.setJobs([own]);
 assert.equal(await stopped(h),"Stopped without reading the release ledger; any claim of this run waits for handler reconciliation.\n");assert.equal(unclaims(h).length,0);
 // The predicate alone: null on any doubt about its inputs.
 const base={jobs:[own],details:new Map([[own.id,own]]),agent:CODE_AGENT,run:CODE_RUN,journalDirectory:h.home,lock:lockOf(r.cwd),matrixChildren:0};
 assert.equal(unclaimableJob(base),"rel_held");
 for(const over of [{jobs:[]},{details:new Map()},{agent:undefined},{run:""},{matrixChildren:1},{matrixChildren:undefined},{journalDirectory:join(h.home,"absent")},{details:new Map([[own.id,{...own,commit:"short"}]])},{details:new Map([[own.id,{...own,batch:{id:"rbt_x"}}]])},{details:null}])assert.equal(unclaimableJob({...base,...over}),null,JSON.stringify(Object.keys(over)));
});
test("stop3 setAsideJournal archives an unclaimed job's exact no-effect journal and holds any other",t=>{
 const home=mkdtempSync(join(tmpdir(),"release-unclaimed-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const path=join(home,"rel_held.json"),record=(disposition,over={})=>({id:"rel_held",state:"verified",generation:3,commit:"c".repeat(40),reconciliations:[{disposition,jobId:"rel_held",agentId:CODE_AGENT,runId:CODE_RUN}],...over});
 const write=(over={})=>{writeFileSync(path,JSON.stringify({version:1,jobId:"rel_held",commit:"c".repeat(40),agentId:CODE_AGENT,runId:CODE_RUN,phase:"waiting_inputs",effects:[],...over}));return readFileSync(path,"utf8");};
 let bytes=write();assert.equal(setAsideJournal(path,record("unclaimed")),"archived");assert.equal(existsSync(path),false);assert.equal(readFileSync(path+".unclaimed-g3","utf8"),bytes);
 bytes=write();assert.equal(setAsideJournal(path,record("set_aside",{generation:4})),"archived");assert.equal(readFileSync(path+".set-aside-g4","utf8"),bytes);
 for(const [name,over,job] of [["an effect",{effects:[{target:"hub"}]}],["published",{published:true}],["another run",{runId:"run_other"}],["another agent",{agentId:"agt_other"}],["another commit",{commit:"d".repeat(40)}],["another job",{jobId:"rel_other"}]]){
  bytes=write(over);assert.equal(setAsideJournal(path,job||record("unclaimed",{generation:5})),"held",name);assert.equal(readFileSync(path,"utf8"),bytes,name);assert.equal(existsSync(path+".unclaimed-g5"),false,name);
 }
 bytes=write();
 for(const job of [record("requeue"),record("unclaimed",{state:"claimed"}),{...record("unclaimed"),reconciliations:[]}])assert.equal(setAsideJournal(path,job),"none");
 assert.equal(readFileSync(path,"utf8"),bytes);
});
// An unclaim the hub saved whose reply was lost or failed validation
// (wi_9a8037eb1a0d543e, order #39143): reply is the fake hub's answer after
// saving it, and reread, when set, answers the one get that follows.
const savedUnclaimHook=(reply,reread="")=>`if(a[1]==='get'&&fs.existsSync(require('path').join(require('path').dirname(process.argv[1]),'unclaimed'))){${reread}}
if(a[1]==='unclaim'){${unclaimSave}fs.writeFileSync(require('path').join(require('path').dirname(process.argv[1]),'unclaimed'),'');${reply}}`;
const rereadAs=over=>`console.log(JSON.stringify({...jobs.find(j=>j.id==='rel_held'),...${over}}));process.exit(0);`;
// The gets made after the unclaim call: the stop's one re-read, or none.
const rereads=h=>{const argvs=h.calls().map(c=>c.argv),i=argvs.findIndex(a=>a[1]==="unclaim");return argvs.slice(i+1).filter(a=>a[1]==="get");};
test("stop5 an unclaim that throws is followed by one re-read: returned only when the hub shows this run's unclaim saved",async t=>{
 const LOST="process.exit(1);",RETURNED="Release rel_held returned to verified at stop; journal archived.\n",REREAD=[["deployment","get","--job","rel_held"]];
 const returned=[
  ["the unclaim reply is lost",LOST],
  ["the unclaim reply is not JSON","console.log('{');process.exit(0);"],
  ["the unclaim reply fails validation","console.log(JSON.stringify({...next,generation:next.generation+5}));process.exit(0);"],
 ];
 for(const [name,reply] of returned){
  const r=codeRepo(t),h=codeHost(t,r.cwd,savedUnclaimHook(reply)),own=heldOwn();waitingJournal(h,own);h.setJobs([own]);
  const before=readFileSync(join(h.home,"rel_held.json"),"utf8");
  assert.equal(await stopped(h),RETURNED,name);
  assert.equal(unclaims(h).length,1,name);assert.deepEqual(rereads(h),REREAD,name);
  assert.equal(existsSync(join(h.home,"rel_held.json")),false,name);assert.equal(readFileSync(join(h.home,"rel_held.json.unclaimed-g3"),"utf8"),before,name);
  const [freed]=ledger(h);assert.deepEqual([freed.state,freed.generation,freed.agentId,freed.runId],["verified",3,undefined,undefined],name);
 }
 // [name, hook, the gets after the unclaim]: the job stays held and its journal untouched.
 const held=[
  ["the hub refuses the unclaim, so the re-read shows it still claimed by this run",unclaimHook(true)],
  ["the re-read fails",savedUnclaimHook(LOST,"process.exit(1);")],
  ["the re-read fails in transport and is not retried",savedUnclaimHook(LOST,"process.stderr.write('tt: hub: 503\\n');process.exit(1);")],
  ["the re-read is not JSON",savedUnclaimHook(LOST,"console.log('{');process.exit(0);")],
  ["the re-read shows the job claimed again",savedUnclaimHook(LOST,rereadAs(`{state:'claimed',generation:4,agentId:'agt_other',runId:'run_other'}`))],
  ["the re-read shows it verified naming a run",savedUnclaimHook(LOST,rereadAs(`{agentId:${JSON.stringify(CODE_AGENT)},runId:${JSON.stringify(CODE_RUN)}}`))],
  ["the re-read shows it at the claim's generation",savedUnclaimHook(LOST,rereadAs("{generation:2}"))],
  ["the re-read shows another commit",savedUnclaimHook(LOST,rereadAs(`{commit:'${"d".repeat(40)}'}`))],
  ["the re-read is a summary",savedUnclaimHook(LOST,rereadAs("{summary:true}"))],
  ["the re-read's last record is another run's unclaim",savedUnclaimHook(LOST,rereadAs("{reconciliations:[{disposition:'unclaimed',jobId:'rel_held',agentId:'agt_other',runId:'run_other'}]}"))],
  ["the re-read's last record is a set-aside",savedUnclaimHook(LOST,rereadAs(`{reconciliations:[{disposition:'set_aside',jobId:'rel_held',agentId:${JSON.stringify(CODE_AGENT)},runId:${JSON.stringify(CODE_RUN)}}]}`))],
  ["the re-read has no records",savedUnclaimHook(LOST,rereadAs("{reconciliations:[]}"))],
 ];
 for(const [name,hook] of held){
  const r=codeRepo(t),h=codeHost(t,r.cwd,hook),own=heldOwn();waitingJournal(h,own);h.setJobs([own]);
  const journal=readFileSync(join(h.home,"rel_held.json"),"utf8");
  assert.equal(await stopped(h),HELD_LINE,name);
  assert.equal(unclaims(h).length,1,name);assert.deepEqual(rereads(h),REREAD,name);
  assert.equal(readFileSync(join(h.home,"rel_held.json"),"utf8"),journal,name);
  assert.deepEqual(readdirSync(h.home).filter(n=>n.includes(".unclaimed-")),[],name);assert.equal(h.claims().length,0,name);
 }
 // A successful unclaim is not re-read.
 const r=codeRepo(t),h=codeHost(t,r.cwd,unclaimHook()),own=heldOwn();waitingJournal(h,own);h.setJobs([own]);
 assert.equal(await stopped(h),RETURNED);assert.equal(unclaims(h).length,1);assert.deepEqual(rereads(h),[]);
 assert.equal(existsSync(join(h.home,"rel_held.json.unclaimed-g3")),true);
});
test("stop4 a stop signal during the 30 s wait ends the daemon without waiting it out",async t=>{
 const r=codeRepo(t),h=codeHost(t,r.cwd),stop=new AbortController();h.setJobs([]);
 const began=Date.now(),timer=setTimeout(()=>stop.abort(),300);t.after(()=>clearTimeout(timer));
 assert.equal(await h.poll(undefined,undefined,{signal:stop.signal}),"");
 assert.ok(Date.now()-began<10000,`ended after ${Date.now()-began} ms`);assert.ok(h.calls().every(c=>c.argv[1]==="list"));
});

// Release batches run the union of their members' checks (wi_8193aff1439e9b20):
// a member whose approved checks include ids the lead lacks joins, the one
// matrix run keeps every joined job's approved checks, and the journal names
// the union, each member's contribution and, on a failed run, whose checks failed.
import {batchAcceptedChecks,batchId,releaseError,failureReason,readBatchSolo,failedChecks} from "../scripts/release-runner.mjs";
import {digest as matrixDigest} from "../scripts/verify-matrix.mjs";
import {planned,GO_CHECKS,BROWSER_CHECK,batchFake,batchFiles,matrixHost} from "./release-runner-fixtures.mjs";
const NPM={id:"npm-unit",argv:["npm","test"],cwd:".",environment:{}},GO_VET=GO_CHECKS[0];
const RACE=(...packages)=>({id:"go-race",argv:["go","test","-race",...packages],cwd:"hub",environment:{}});
const unionIds=["npm-unit","go-vet","go-test","tests/board-browser.mjs"];
const receiptOf=(c,journal)=>JSON.parse(readFileSync(join(c.home,journal.batch.id+"-receipt.json"),"utf8"));
test("union1 a member with check ids the lead lacks joins, and the batch's checks are the union, each once",()=>{
 const b=planned([NPM],[NPM,...GO_CHECKS],[NPM,BROWSER_CHECK]),[a,c]=b.members;
 assert.equal(batchMismatch(b.lead,a),"");assert.equal(batchMismatch(b.lead,c),"");
 const merged=batchAcceptedChecks(b.lead,[a,c]);
 assert.deepEqual(merged.joined,["rel_b2","rel_b3"]);assert.deepEqual(merged.dropped,[]);
 assert.deepEqual(merged.checks.map(x=>x.id),unionIds);assert.deepEqual(merged.checks,[NPM,...GO_CHECKS,BROWSER_CHECK]);assert.equal(merged.checksDigest,matrixDigest(merged.checks));
 // go-race joins by package union when the lead has none, and ./... wins.
 const race=planned([NPM],[NPM,RACE("./internal/api")],[NPM,RACE("./internal/store")]);
 assert.deepEqual(batchAcceptedChecks(race.lead,race.members).checks,[NPM,RACE("./internal/api","./internal/store")]);
 const all=planned([NPM],[NPM,RACE("./internal/api")],[RACE("./...")]);
 assert.deepEqual(batchAcceptedChecks(all.lead,all.members).checks,[NPM,RACE("./...")]);
});
test("union2 a batch of two members with disjoint extra checks runs the union once, covers every job and records it",async()=>{
 const b=planned([NPM],[NPM,...GO_CHECKS],[NPM,BROWSER_CHECK]),a=batchFake(b.lead),c=batchConfig(b,3);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");
 assert.deepEqual(a.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3","verify","merged","deploy:tailos","finish"]);
 assert.equal(a.verifies.length,1);const handed=a.verifies[0];
 assert.deepEqual(handed.plan.checks.map(x=>x.id),unionIds);assert.equal(handed.plan.checksDigest,matrixDigest(handed.plan.checks));
 // Every job's approved checks are covered by the union; the lead's own plan leaves rel_b2 and rel_b3 uncovered.
 const all=covers(b.lead,...b.members);
 assert.equal(batchPlanCovers({checks:handed.plan.checks},all),null);assert.equal(batchPlanCovers({checks:handed.plan.checks},handed.batchCovers),null);
 assert.deepEqual(batchPlanCovers({checks:b.lead.plan.checks},all),{jobId:"rel_b2",checkId:"go-vet"});
 assert.deepEqual(batchPlanCovers({checks:b.lead.plan.checks},covers(b.members[1])),{jobId:"rel_b3",checkId:"tests/board-browser.mjs"});
 const journal=batchJournal(c),receipt=receiptOf(c,journal);
 assert.deepEqual(receipt.checks,unionIds);assert.equal(receipt.checksDigest,handed.plan.checksDigest);
 assert.deepEqual(receipt.jobs.map(j=>[j.jobId,j.checkIds,j.contributed]),[["rel_b1",["npm-unit"],undefined],["rel_b2",["npm-unit","go-vet","go-test"],["go-vet","go-test"]],["rel_b3",["npm-unit","tests/board-browser.mjs"],["tests/board-browser.mjs"]]]);
 assert.deepEqual(receipt.dropped,[]);assert.deepEqual(c.solo,[]);
});
test("union3 the real integrated plan keeps a docs lead's and a Go member's checks and covers both",async()=>{
 const m=matrixRepo(),lead=m.accept(1,"docs/a.md","a\n"),member=m.accept(2,"hub/internal/store/store.go","package store\n\nvar Member = 1\n");m.settle();
 ignorePrerequisites(m.f.cwd);placePrerequisites(m.f.cwd);
 assert.deepEqual(checkIds(lead.plan),["npm-unit"]);assert.deepEqual(checkIds(member.plan),["go-race","go-test","go-vet"]);assert.equal(batchMismatch(lead,member),"");
 const a=matrixFake(lead),c=batchConfig({f:m.f,lead,members:[member]},2);
 const r=await runRelease(c,a);assert.equal(r.outcome,"released");assert.equal(a.verifies.length,1);
 const handed=a.verifies[0];assert.deepEqual(checkIds(handed.plan),["npm-unit","go-race","go-test","go-vet"]);
 const h=await hostPlan(m,handed);
 assert.deepEqual(checkIds(h.plan),["go-race","go-test","go-vet","npm-unit"]);
 assert.equal(batchPlanCovers(h.plan,handed.batchCovers),null);assert.equal(batchPlanCovers(h.plan,covers(lead,member)),null);assert.equal(h.started.length,1);
});
test("union4 eligibility is unchanged, and a same-id check that differs drops the later job, the lead's or an earlier member's",async()=>{
 const b=planned([NPM,GO_VET,RACE("./internal/api")],[NPM,GO_VET]),member=b.members[0],edited=(edit,lead=b.lead)=>{const j=structuredClone(member);edit(j);return batchMismatch(lead,j);};
 assert.equal(batchMismatch(b.lead,member),"");
 assert.equal(edited(j=>j.repository="other"),"batch-ineligible");assert.equal(edited(j=>j.baseCommit="e".repeat(40)),"batch-ineligible");assert.equal(edited(j=>j.pauseGeneration=2),"batch-ineligible");
 assert.equal(edited(j=>j.plan.matrixDigest="e".repeat(64)),"batch-other-matrix");assert.equal(edited(j=>j.plan.matrixApprovalMessageSeq=10),"batch-other-matrix");
 assert.equal(edited(j=>j.plan.checks[1].argv.push("-x")),"batch-plan-mismatch");assert.equal(edited(j=>j.plan.checks[1].cwd="."),"batch-plan-mismatch");assert.equal(edited(j=>j.plan.checks[1].environment={X:"1"}),"batch-plan-mismatch");
 assert.equal(edited(j=>j.plan.checks.push(RACE("./internal/store"))),"");assert.equal(edited(j=>j.plan.checks.push({...RACE("./internal/store"),argv:["go","test","-race","-count=1","./internal/store"]})),"batch-plan-mismatch");
 // Member against member: rel_b3's go-vet differs from the one rel_b2 brought, so rel_b3 is dropped before it is picked.
 const VET_X={...GO_VET,argv:[...GO_VET.argv,"-x"]},m=planned([NPM],[NPM,GO_VET],[NPM,VET_X]);
 assert.equal(batchMismatch(m.lead,m.members[1]),"","the lead alone has no go-vet");
 assert.deepEqual(batchAcceptedChecks(m.lead,m.members).dropped,[{jobId:"rel_b3",reason:"batch-plan-mismatch"}]);
 const a=batchFake(m.lead),c=batchConfig(m,3);
 assert.equal((await runRelease(c,a)).outcome,"released");
 assert.deepEqual(a.calls.filter(x=>x.startsWith("batch-")),["batch-open","batch-add:tqe_b1","batch-add:tqe_b2"]);
 const journal=batchJournal(c),dropped=[{jobId:"rel_b3",reason:"batch-plan-mismatch"}];
 assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b2"]);assert.deepEqual(journal.batchDropped,dropped);assert.deepEqual(receiptOf(c,journal).dropped,dropped);
 assert.deepEqual(a.verifies[0].plan.checks,[NPM,GO_VET]);assert.deepEqual(c.solo,[]);
 // A refused member contributes nothing, so the job behind it then joins with its own go-vet.
 const n=planned([NPM],[NPM,GO_VET],[NPM,VET_X]),a2=batchFake(n.lead,{refuse:(op,fields)=>op==="batch-add" && fields.entryId==="tqe_b2"}),c2=batchConfig(n,3);
 assert.equal((await runRelease(c2,a2)).outcome,"released");
 const j2=batchJournal(c2);
 assert.deepEqual(j2.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b3"]);assert.deepEqual(j2.batchDropped,[{jobId:"rel_b2",reason:"batch-add-refused"}]);
 assert.deepEqual(a2.verifies[0].plan.checks,[NPM,VET_X]);assert.deepEqual(receiptOf(c2,j2).checks,["npm-unit","go-vet"]);
});
test("union5 a failed union run names, for each failed check, the jobs whose approved checks include it, and every member still releases alone",async()=>{
 const run=async(failure)=>{
  const b=planned([NPM],[NPM,...GO_CHECKS],[NPM,BROWSER_CHECK]),a=batchFake(b.lead),c=batchConfig(b,3);
  a.verifyIntegrated=async j=>{a.calls.push("verify");a.verifies.push(j);if(a.verifies.length===1)throw Object.assign(releaseError("Integrated matrix receipt is not eligible"),failure);return true;};
  assert.equal((await runRelease(c,a)).outcome,"released");
  assert.deepEqual(a.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3","verify","batch-drop","verify","merged","deploy:tailos","finish"]);
  assert.deepEqual(c.solo,["rel_b2","rel_b3"]);assert.deepEqual([...readBatchSolo(c.home)],["rel_b2","rel_b3"]);assert.deepEqual(batchFiles(c.home),["batch-solo.json"]);
  const journal=batchJournal(c);assert.equal(journal.batch,null);
  return {batchId:batchId("rel_b1",2,b.tip),fallback:journal.batchFallback};
 };
 const base={reason:"batch-matrix-failed",detail:"Integrated matrix receipt is not eligible",members:["rel_b2","rel_b3"],status:"done"};
 let r=await run({failedChecks:["go-vet"],unnamed:0});
 assert.deepEqual(r.fallback,{batchId:r.batchId,...base,failed:[{checkId:"go-vet",jobs:["rel_b2"]}],unaffected:["rel_b1","rel_b3"]});
 r=await run({failedChecks:["npm-unit","wasm-test-build"],unnamed:2});
 assert.deepEqual(r.fallback,{batchId:r.batchId,...base,failed:[{checkId:"npm-unit",jobs:["rel_b1","rel_b2","rel_b3"]},{checkId:"wasm-test-build",jobs:[]}],unaffected:[],unnamed:2});
 // A failure that names no checks keeps today's fallback record.
 r=await run({});
 assert.deepEqual(r.fallback,{batchId:r.batchId,...base});
 // The host adapter names a receipt's failed checks, validated and counted.
 const h=matrixHost({receipt:{environment:{},checks:[{id:"go-vet",exitCode:1},{id:"npm-unit",exitCode:0},{id:"k",exitCode:1,knownFailure:true},{exitCode:1},{id:"bad id;",exitCode:2}]}});
 await assert.rejects(h.verify(h.integrated),e=>failureReason(e)==="Integrated matrix receipt is not eligible" && e.message==="Integrated matrix receipt is not eligible" && JSON.stringify(e.failedChecks)==='["go-vet"]' && e.unnamed===2);
 assert.equal(h.sends().length,0);
 const many=Array.from({length:60},(_,i)=>({id:"c"+i,exitCode:1}));
 assert.deepEqual(failedChecks({checks:many}),{failedChecks:many.slice(0,50).map(c=>c.id),unnamed:10});
 assert.deepEqual(failedChecks({checks:[{id:"a",exitCode:0}]}),{failedChecks:[],unnamed:0});
});
