import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,chmodSync,existsSync,readdirSync,rmSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {heldClaimNotice,runRelease,HostAdapter,serveDeployment,releaseError,validateNativeRelease,failureReason,matrixWaitNotice,matrixHeldNotice,fenceWaitNotice,CODE_REASONS,codeNotice,cliFailureNotice,WAKE} from "../scripts/release-runner.mjs";

// Release batching (wi_50e80d45647b342a slice one).
import {integrateBatch,batchCandidates,batchAcceptedChecks,batchMismatch,batchPlanCovers,batchId,releaseBatchPolicy,materializeBatch,readBatchSolo,reconcileReceipts,BATCH_IMPORT_WAIT_MS} from "../scripts/release-runner.mjs";
import {planWithPreservation,digest as matrixDigest} from "../scripts/verify-matrix.mjs";
import {git,change,job,recorded,RUNNER,ignorePrerequisites,placePrerequisites,exitedPid,until,ended,idle,CODE_RUN,codeRepo,codeHost,verifiedJob,codeGate,HELD_ITEM,HELD_HANDLER,HELD_HELPER,heldRoster,heldRun,heldOwn,heldJournal,HELD_SUBJECT,DRAINING_SUBJECT,batchJobs,batchFake,batchConfig,batchJournal,batchFiles,three,clean,matrixRepo,race,integratedPlan,covers,matrixFake,batchHub,daemonHub,SINGLE_JOB_TRACE,traceOf,GO_CHECKS,planned,daemonJournal,MISMATCH,stale,claimed,wakeRefs,noticeArgv,argvField,WAKE_WAIT,WAKE_HELD,untaggedNoticeSends} from "./release-runner-fixtures.mjs";
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
 // A check id the lead lacks joins the union.
 const extra=structuredClone(code);extra.plan.checks.push({id:"npm-unit",argv:["npm","test"],cwd:".",environment:{}});assert.equal(batchMismatch(lead,extra),"");
 const union=batchAcceptedChecks(lead,[extra]);assert.deepEqual(union.joined,["rel_m3"]);assert.deepEqual(union.checks.map(c=>c.id),[...lead.plan.checks.map(c=>c.id),"npm-unit"]);
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
// A member whose go-vet has the lead's id and other argv cannot share its run.
const VET_X=[{...GO_CHECKS[0],argv:[...GO_CHECKS[0].argv,"-x"]},GO_CHECKS[1]];
test("batch16 a lead records batch-plan-mismatch for a member whose same-id check differs, and a matching member still joins",async()=>{
 // The lead goes alone: the reason is in its journal though no batch formed.
 const alone=planned(GO_CHECKS,VET_X),h=daemonHub(alone,{releaseBatch:{maxJobs:3}});
 await h.poll();
 assert.deepEqual(h.seen[0].batch.candidates,[]);assert.deepEqual(h.seen[0].batch.excluded,MISMATCH);
 let journal=daemonJournal(h);
 assert.deepEqual(journal.batchDropped,MISMATCH);assert.equal(journal.phase,"complete");
 for(const key of ["batch","batchOps","batchLead","batchReceipt","batchFallback"])assert.ok(!(key in journal),key);
 assert.deepEqual(h.read().batch,[]);assert.deepEqual(h.read().jobs.map(j=>[j.id,j.state]),[["rel_b1","released"],["rel_b2","verified"]]);assert.deepEqual(batchFiles(h.home),[]);
 // A matching member behind the mismatched one joins, and the batch receipt carries the same reason.
 const mixed=planned(GO_CHECKS,VET_X,GO_CHECKS),g=daemonHub(mixed,{releaseBatch:{maxJobs:3}});
 await g.poll();
 journal=daemonJournal(g);
 assert.deepEqual(g.seen[0].batch.candidates.map(j=>j.id),["rel_b3"]);assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b3"]);assert.deepEqual(journal.batchDropped,MISMATCH);
 assert.deepEqual(g.read().batch,["batch-open","batch-add:tqe_b1","batch-add:tqe_b3"]);assert.deepEqual(g.read().jobs.map(j=>[j.id,j.state,j.generation]).slice(1),[["rel_b2","verified",1],["rel_b3","verified",1]]);
 const receipt=JSON.parse(readFileSync(join(g.home,journal.batch.id+"-receipt.json"),"utf8"));
 assert.deepEqual(receipt.jobs.map(j=>j.jobId),["rel_b1","rel_b3"]);assert.deepEqual(receipt.dropped,MISMATCH);assert.equal(receipt.outcome,"released");
 // The other reasons a full record gives are recorded the same way.
 const kinds=planned(undefined,undefined,undefined);kinds.members[0].repository="other";kinds.members[1].plan.matrixDigest="e".repeat(64);
 const k=daemonHub(kinds,{releaseBatch:{maxJobs:3}});await k.poll();
 assert.deepEqual(daemonJournal(k).batchDropped,[{jobId:"rel_b2",reason:"batch-ineligible"},{jobId:"rel_b3",reason:"batch-other-matrix"}]);assert.ok(!("batch" in daemonJournal(k)));
});
test("batch17 an unreadable candidate and those past the read limit are recorded",async()=>{
 const files=[["client/a.js","a"],["client/b.js","b"],["client/c.js","c"],["client/d.js","d"],["client/e.js","e"]];
 // The record of rel_b2 cannot be read: it is left out and the next one joins.
 const h=daemonHub(batchJobs(files.slice(0,3)),{releaseBatch:{maxJobs:3},unreadable:["rel_b2"]});
 await h.poll();
 let journal=daemonJournal(h);const unreadable=[{jobId:"rel_b2",reason:"detail-unreadable"}];
 assert.deepEqual(journal.batchDropped,unreadable);assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b3"]);
 assert.deepEqual(JSON.parse(readFileSync(join(h.home,journal.batch.id+"-receipt.json"),"utf8")).dropped,unreadable);
 // A size of 2 reads two candidates; the two behind them are over the limit and were never read.
 const g=daemonHub(batchJobs(files),{releaseBatch:{maxJobs:2}});
 await g.poll();
 journal=daemonJournal(g);const over=[{jobId:"rel_b4",reason:"over-limit"},{jobId:"rel_b5",reason:"over-limit"}];
 assert.deepEqual(g.seen[0].batch.candidates.map(j=>j.id),["rel_b2","rel_b3"]);assert.deepEqual(g.seen[0].batch.excluded,over);
 assert.deepEqual(journal.batchDropped,over);assert.deepEqual(journal.batch.jobs.map(j=>j.jobId),["rel_b1","rel_b2"]);
 assert.deepEqual(g.read().log.filter(l=>l.startsWith("deployment get")).map(l=>l.split(" ")[3]),["rel_b1","rel_b2","rel_b3"]);
 assert.deepEqual(JSON.parse(readFileSync(join(g.home,journal.batch.id+"-receipt.json"),"utf8")).dropped,over);
 // Nothing left out: the journal gains no key.
 const n=daemonHub(batchJobs(files.slice(0,2)),{releaseBatch:{maxJobs:3}});await n.poll();
 assert.deepEqual(n.seen[0].batch.excluded,[]);assert.ok(!("batchDropped" in daemonJournal(n)));
});
test("batch18 an exclusion is recorded once per claim, over repeated polls and a resumed run",async()=>{
 // The matrix run is not over for two polls, so the same claim is polled three times.
 for(const checks of [[GO_CHECKS,VET_X],[GO_CHECKS,VET_X,GO_CHECKS]]){
  const b=planned(...checks),h=daemonHub(b,{releaseBatch:{maxJobs:3}});let polls=0;
  const release=(c,adapter)=>{const run=h.release(c,adapter),wait=++polls<3;adapter.verifyIntegrated=async()=>!wait;return run;};
  const poll=()=>serveDeployment(h.config,{once:true,configPath:h.configPath,release});
  await poll();
  assert.equal(daemonJournal(h).phase,"waiting_matrix");assert.deepEqual(daemonJournal(h).batchDropped,MISMATCH);assert.equal(h.read().jobs[0].state,"claimed");
  await poll();
  assert.equal(daemonJournal(h).phase,"waiting_matrix");assert.deepEqual(daemonJournal(h).batchDropped,MISMATCH);
  await poll();
  assert.equal(polls,3);assert.equal(h.read().jobs[0].state,"released");assert.equal(h.read().log.filter(l=>l.startsWith("deployment claim")).length,1,"one claim");
  const journal=daemonJournal(h);assert.equal(journal.phase,"complete");assert.deepEqual(journal.batchDropped,MISMATCH);
  // A batch is read again at each poll and hands the same exclusion over; the lead alone hands none.
  assert.deepEqual(h.seen.map(c=>c.batch.excluded),checks.length===3?[MISMATCH,MISMATCH,MISMATCH]:[MISMATCH,[],[]]);
  if(checks.length===3)assert.deepEqual(JSON.parse(readFileSync(join(h.home,journal.batch.id+"-receipt.json"),"utf8")).dropped,MISMATCH);
 }
 // A candidate the daemon left out and the runner then refuses again is listed once, with its first reason.
 const b=planned(GO_CHECKS,VET_X,GO_CHECKS),a=batchFake(b.lead),c=batchConfig(b,3,{excluded:[...MISMATCH,...MISMATCH]});
 const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");assert.deepEqual(batchJournal(c).batchDropped,MISMATCH);assert.deepEqual(batchJournal(c).batch.jobs.map(j=>j.jobId),["rel_b1","rel_b3"]);
 // A size of 1 records nothing even if exclusions are handed over.
 const one=planned(GO_CHECKS,VET_X),a1=batchFake(one.lead),c1=batchConfig(one,1,{candidates:[],excluded:MISMATCH});
 assert.equal((await runRelease(c1,a1)).outcome,"released");assert.ok(!("batchDropped" in batchJournal(c1)));
});
test("batch19 a batch with an unknown path starts no matrix run and falls back to per-job releases",async()=>{
 // A top-level file no map rule knows (the batch fixture creates no folders).
 const UNKNOWN="newthing.cfg",REASON="Unknown release path: "+UNKNOWN,fellBack=(b,status="done")=>({batchId:batchId("rel_b1",2,b.tip),reason:"batch-selection-refused",detail:REASON,members:["rel_b2","rel_b3"],status});
 const alone=(b,member)=>{const lead={...member,state:"claimed",generation:2,agentId:"agt_fixture",runId:"run_fixture"},a=batchFake(lead),c=batchConfig({f:b.f,lead,members:[]},3);return {a,c,run:()=>runRelease(c,a)};};
 // A member adds the path: the batch is dropped before any matrix run and the lead goes on alone.
 const b=batchJobs([["client/a.js","a"],[UNKNOWN,"b"],["client/c.js","c"]]),a=batchFake(b.lead),c=batchConfig(b,3);
 const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");
 assert.deepEqual(a.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3","batch-drop","verify","merged","deploy:tailos","finish"]);
 assert.equal(a.verifies.length,1,"the only matrix run is the lead's own");assert.equal(a.verifies[0].batchCovers,undefined);assert.equal(a.verifies[0].integratedCommit,r.commit);
 let journal=batchJournal(c);
 assert.deepEqual(journal.batchFallback,fellBack(b));assert.equal(journal.batch,null);assert.equal(journal.batchReceipt,undefined);assert.equal(journal.batchSelected,undefined);assert.equal(journal.failure,undefined);assert.equal(journal.revert,undefined);
 assert.deepEqual(c.solo,["rel_b2","rel_b3"]);assert.deepEqual([...readBatchSolo(c.home)],["rel_b2","rel_b3"]);assert.deepEqual(batchFiles(c.home),["batch-solo.json"]);
 assert.deepEqual(git(b.f.cwd,"diff","--name-only",b.tip,r.commit).split("\n"),["client/a.js"]);assert.equal(git(b.f.cwd,"rev-parse","refs/heads/tasks-hub"),r.commit);
 // The member with the path is refused alone, by name, before its matrix run; nothing of it is published.
 const m2=alone(b,b.members[0]);await assert.rejects(m2.run(),/refused/);
 assert.deepEqual(m2.a.calls,["refuse","escalate"]);assert.equal(m2.a.verifies.length,0);
 journal=batchJournal(m2.c);assert.equal(journal.phase,"refused");assert.equal(journal.refusalReason,REASON);assert.deepEqual(journal.failure,{step:"select",reason:REASON});
 assert.notEqual(journal.published,true);assert.deepEqual(journal.effects,[]);assert.equal(journal.revert,undefined);assert.equal(journal.batch,undefined);
 assert.equal(git(b.f.cwd,"rev-parse","refs/heads/tasks-hub"),r.commit);
 // The other member releases alone.
 const m3=alone(b,b.members[1]),r3=await m3.run();
 assert.equal(r3.outcome,"released");assert.deepEqual(m3.a.calls,["verify","merged","deploy:tailos","finish"]);
 assert.deepEqual(git(b.f.cwd,"diff","--name-only",b.tip,r3.commit).split("\n"),["client/a.js","client/c.js"]);
 assert.equal(git(b.f.cwd,"log","--format=%s",b.tip+"..refs/heads/tasks-hub").split("\n").filter(x=>/revert/i.test(x)).length,0,"nothing was reverted");
 assert.equal(git(b.f.cwd,"ls-tree","--name-only","refs/heads/tasks-hub",UNKNOWN),"");
 // The lead adds the path: it is refused by name and the members still release alone.
 const l=batchJobs([[UNKNOWN,"b"],["client/b.js","b"],["client/c.js","c"]]),la=batchFake(l.lead),lc=batchConfig(l,3);
 await assert.rejects(runRelease(lc,la),/refused/);
 assert.deepEqual(la.calls,["batch-open","batch-add:tqe_b1","batch-add:tqe_b2","batch-add:tqe_b3","batch-drop","refuse","escalate"]);assert.equal(la.verifies.length,0);
 journal=batchJournal(lc);assert.deepEqual(journal.failure,{step:"select",reason:REASON});assert.equal(journal.refusalReason,REASON);assert.deepEqual(journal.batchFallback,fellBack(l));
 assert.deepEqual(lc.solo,["rel_b2","rel_b3"]);assert.equal(git(l.f.cwd,"rev-parse","refs/heads/tasks-hub"),l.tip);
 for(const [i,member] of l.members.entries()){const m=alone(l,member);assert.equal((await m.run()).outcome,"released");assert.equal(git(l.f.cwd,"show",`refs/heads/tasks-hub:client/${"bc"[i]}.js`),"bc"[i]);}
 // The hub refuses the drop: the lead is refused with the selection's reason, still before any matrix run.
 const d=batchJobs([["client/a.js","a"],[UNKNOWN,"b"],["client/c.js","c"]]),da=batchFake(d.lead,{refuse:op=>op==="batch-drop"}),dc=batchConfig(d,3);
 await assert.rejects(runRelease(dc,da),/refused/);
 assert.equal(da.verifies.length,0);assert.deepEqual(da.calls.slice(-2),["refuse","escalate"]);assert.ok(!da.calls.includes("merged"));
 journal=batchJournal(dc);assert.deepEqual(journal.failure,{step:"select",reason:REASON});assert.deepEqual(journal.batchFallback,fellBack(d,"refused"));assert.equal(git(d.f.cwd,"rev-parse","refs/heads/tasks-hub"),d.tip);
 // A batch whose selection passed saves it, and a resumed wait does not select again beside its run.
 const w=three(),wa=batchFake(w.lead),wc=batchConfig(w,3);let ready=false;
 wa.verifyIntegrated=async j=>{wa.calls.push("verify");wa.verifies.push(j);return ready;};
 assert.deepEqual(await runRelease(wc,wa),{jobId:"rel_b1",outcome:"waiting_matrix"});
 journal=batchJournal(wc);assert.equal(journal.batchSelected,journal.integrated);assert.equal(journal.batchFallback,undefined);
 ready=true;assert.equal((await runRelease(wc,wa)).outcome,"released");assert.deepEqual(wa.calls.slice(-4),["verify","merged","deploy:tailos","finish"]);assert.ok(!wa.calls.includes("batch-drop"));
 // A fallback stopped while dropping resumes the drop and starts no batch run.
 const s=batchJobs([["client/a.js","a"],[UNKNOWN,"b"],["client/c.js","c"]]),sa=batchFake(s.lead),sc=batchConfig(s,3);let over=false;
 sa.settleMatrixRuns=async()=>sa.ops.length?over:true;
 assert.deepEqual(await runRelease(sc,sa),{jobId:"rel_b1",outcome:"waiting_matrix"});
 assert.equal(batchJournal(sc).batchFallback.status,"dropping");assert.ok(!sa.calls.includes("batch-drop"));assert.equal(sa.verifies.length,0);
 over=true;const sr=await runRelease(sc,sa);
 assert.equal(sr.outcome,"released");assert.deepEqual(sa.calls.slice(-5),["batch-drop","verify","merged","deploy:tailos","finish"]);assert.equal(sa.verifies.length,1);assert.equal(sa.verifies[0].batchCovers,undefined);
 assert.deepEqual(batchJournal(sc).batchFallback,fellBack(s));assert.deepEqual(sc.solo,["rel_b2","rel_b3"]);
});
test("wake a1 each recipient-less notice has its class at the builder and exactly one wake ref in the argv its sender passes",async t=>{
 const R=WAKE.routine,A=WAKE.attention;assert.deepEqual(WAKE,{routine:"routine",attention:"attention"});
 // rows[n]: the class of plan row n, what its builder returned, and each argv its sender passed.
 const rows=new Map(),row=(n,cls,built,argvs)=>rows.set(n,{cls,built,argvs});
 const plain=()=>{const cwd=mkdtempSync(join(tmpdir(),"release-wake-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));return cwd;};
 const bySubject=(h,subject)=>noticeArgv(h).filter(a=>argvField(a,"--subject")===subject);
 // Row 1: a failed detail read holds the poll, which then reports the CLI failure.
 {const h=codeHost(t,plain(),`if(a[1]==='get')process.exit(4);`);h.setJobs([verifiedJob()]);await h.poll(null);
  row(1,R,[cliFailureNotice({reason:"tt deployment get exit 4",jobId:"rel_code",at:"2026-10-07T00:00:00.000Z"},CODE_RUN).wake],bySubject(h,"A deployer CLI call failed"));}
 // Rows 2, 3 and 12: a claimed job waits for its matrix import, then for its inputs, with a later job behind it.
 {const h=codeHost(t,plain());h.setJobs([verifiedJob("rel_a"),verifiedJob("rel_b")]);
  for(const outcome of ["waiting_matrix","waiting_inputs"])await h.poll(null,async(c,adapter)=>{adapter.matrixWait=WAKE_WAIT;adapter.matrixHeld=WAKE_HELD;return {outcome};});
  row(2,R,[matrixWaitNotice({id:"rel_a"},WAKE_WAIT).wake],bySubject(h,"A release job is waiting for the verification host"));
  row(3,A,[matrixHeldNotice({id:"rel_a"},WAKE_HELD).wake,matrixHeldNotice({id:"rel_a"},{...WAKE_HELD,attempt:"attempts"}).wake],bySubject(h,"A release job is held until its matrix run is confirmed stopped"));
  const fence=bySubject(h,"A release job is waiting behind a held project fence");
  assert.deepEqual(fence.map(a=>argvField(a,"--request-id")),["rel_a-fence-wait-rel_b-waiting_matrix","rel_a-fence-wait-rel_b-waiting_inputs"]);
  const jobs=[{id:"rel_a",state:"claimed"},{id:"rel_b",state:"verified"}],built=[];
  for(const reason of ["waiting_matrix","waiting_inputs"])for(const holder of [jobs[0],{...jobs[0],inputsDigest:"a".repeat(64)}])for(const locked of [false,true])for(const run of [false,true])built.push(fenceWaitNotice(jobs,holder,reason,locked,run).wake);
  assert.equal(built.length,16);row(12,R,built,fence);}
 // Row 13: a blocked or merged job, or one another run claimed, holds the fence.
 {const argvs=[],built=[];
  for(const state of ["blocked","merged","claimed"]){
   const h=codeHost(t,plain()),holder={id:"rel_hold",state,generation:3,commit:"d".repeat(40),agentId:"agt_other",runId:"run_other"};h.setJobs([holder,verifiedJob("rel_next")]);await h.poll(null);
   const sent=bySubject(h,"A release job is waiting behind a held project fence");assert.equal(sent.length,1,state);assert.match(argvField(sent[0],"--request-id"),new RegExp("^rel_hold-fence-wait-rel_next-"+state));argvs.push(...sent);
   for(const locked of [false,true])for(const run of [false,true])built.push(fenceWaitNotice([holder,verifiedJob("rel_next")],holder,state,locked,run).wake);
  }
  row(13,A,built,argvs);}
 // Rows 4, 5 and 6: the host adapter's three escalations build and send in one step.
 {const cwd=plain(),calls=[],adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
  await adapter.escalate({outcome:"refused",reason:"integration-conflict: 1 path"});await adapter.escalate({outcome:"refused"});await adapter.escalate({push:"failed"});await adapter.escalate({});await adapter.escalate({revert:"failed",rollbackBlocked:true});
  const of=subject=>calls.filter(a=>argvField(a,"--subject")===subject);
  row(4,A,[],of("Release refused before publication"));row(5,A,[],of("Release is live but the tasks-hub push failed"));row(6,A,[],of("Release failed and requires recovery"));
  assert.deepEqual([4,5,6].map(n=>rows.get(n).argvs.length),[2,1,2]);}
 // Row 7: stale code behind a blocked job drains.
 {const r=codeRepo(t),h=codeHost(t,r.cwd),{gate}=codeGate(r.codeA);r.at(r.a,r.b);h.setJobs([{id:"rel_blocked",state:"blocked",generation:3,commit:"d".repeat(40)},verifiedJob("rel_next")]);await h.poll(gate);
  row(7,R,[codeNotice(h.record(),"draining").wake],bySubject(h,DRAINING_SUBJECT));}
 // Row 8: the only owned job is held, so the stale runner restarts with it.
 {const r=codeRepo(t),h=codeHost(t,r.cwd),own=heldOwn(),{gate}=codeGate(r.codeA);r.at(r.a,r.b);h.setJobs([own,verifiedJob("rel_next")]);heldJournal(h,own);await h.poll(gate);
  assert.equal(h.record().heldJob,"rel_held");row(8,R,[codeNotice(h.record(),"restart").wake],bySubject(h,HELD_SUBJECT));}
 // Rows 9 and 11: an idle stale runner announces its restart; the re-exec then fails and it refuses.
 {const r=codeRepo(t),h=codeHost(t,r.cwd),{gate}=codeGate(r.codeA,{execve:()=>{throw new Error("no exec");}});r.at(r.a,r.b);h.setJobs([verifiedJob()]);await h.poll(gate);
  assert.equal(h.record().state,"refused");assert.equal(h.record().reason,"exec-failed");
  row(9,R,[codeNotice({...h.record(),state:"restart"},"restart").wake],bySubject(h,"Deployer is restarting itself onto the published scripts"));
  row(11,A,[...CODE_REASONS,"anything else"].map(reason=>codeNotice({...h.record(),reason},"refused").wake),noticeArgv(h).filter(a=>argvField(a,"--subject").startsWith("Deployer is not claiming releases: ")));}
 // Row 10: the restarted runner reports the scripts it now runs.
 {const r=codeRepo(t),h=codeHost(t,r.cwd),{gate}=codeGate(r.codeB,{marker:r.codeB.digest,restartedFrom:"f".repeat(64)});await h.poll(gate);
  row(10,R,[codeNotice(h.record(),"restarted").wake],bySubject(h,"Deployer now runs the published scripts"));}
 // Row 14: only the Board copy of the held claim notice has a class; the directed copies have none.
 {const refuse=`if(a[1]==='claim'&&flag('--job')==='rel_held'){fs.writeSync(2,'tt: hub: 409 conflict: release: job cannot be taken over\\n');process.exit(1);}`;
  const board=codeHost(t,plain(),refuse+`if(a[0]==='agents'){console.log('[]');process.exit(0);}`),directed=codeHost(t,plain(),refuse+heldRoster);
  for(const h of [board,directed]){h.setJobs([{...verifiedJob("rel_held"),itemId:HELD_ITEM},verifiedJob("rel_next")]);await h.poll(null);}
  const copies=noticeArgv(directed);assert.deepEqual(copies.map(a=>argvField(a,"--to")),[HELD_HANDLER,HELD_HELPER]);for(const a of copies){assert.ok(a.includes("--to"));assert.deepEqual(wakeRefs(a),[]);}
  const sent=noticeArgv(board);assert.equal(sent.length,1);assert.ok(!sent[0].includes("--to"));assert.match(argvField(sent[0],"--request-id"),/-board$/);
  row(14,A,["claim","journal"].map(kind=>heldClaimNotice({id:"rel_held"},kind,"x",CODE_RUN).wake),sent);}
 assert.deepEqual([...rows.keys()].sort((a,b)=>a-b),Array.from({length:14},(_,i)=>i+1));
 const classes={};
 for(const [n,{cls,built,argvs}] of rows){
  classes[n]=cls;assert.ok([4,5,6].includes(n)||built.length>0,`row ${n} has a builder`);assert.ok(argvs.length>0,`row ${n} was sent`);
  for(const wake of built)assert.equal(wake,cls,`row ${n} at its builder`);
  for(const argv of argvs){assert.ok(!argv.includes("--to"),`row ${n} has no recipient`);assert.deepEqual(wakeRefs(argv),[`wake=${cls}`],`row ${n} argv`);}
 }
 assert.deepEqual(classes,{1:R,2:R,3:A,4:A,5:A,6:A,7:R,8:R,9:R,10:R,11:A,12:R,13:A,14:A});
});
test("wake a2 every notice send in the runner passes a wake ref, and a send without one is found",()=>{
 const source=readFileSync(RUNNER,"utf8"),found=untaggedNoticeSends(source);
 assert.equal(found.count,8,"three escalations, fence wait, matrix host, runner code, CLI failure and held claim");assert.deepEqual(found.untagged,[]);
 // No notice is sent in a form the line check cannot see.
 assert.equal(source.split('"notice"').length-1,found.count);assert.ok(!/"--kind",\s*[^"\s]/.test(source),"every kind is a literal");
 // The check bites: one ref removed from a scratch copy, for each send in turn.
 const lines=source.split("\n"),sends=lines.map((text,i)=>i).filter(i=>/"--kind",\s*"notice"/.test(lines[i]));
 for(const i of sends){
  const scratch=[...lines];scratch[i]=scratch[i].replace(/,\.\.\.(?:wakeRef\([A-Za-z.]+\)|\(to\?\[\]:wakeRef\(notice\.wake\)\))\]\)/,"])");assert.notEqual(scratch[i],lines[i]);
  assert.deepEqual(untaggedNoticeSends(scratch.join("\n")),{count:8,untagged:[i+1]});
 }
 assert.deepEqual(untaggedNoticeSends('x.command([tt,"send","--kind","notice","--subject",s]);'),{count:1,untagged:[1]});
});
test("wake a11 a refused send of a tagged notice leaves the poll running, is journaled, is not marked posted and is sent again by the next poll",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-wake-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 // The hub refuses the fence-wait notice as it does a request id reused with other data, until the file exists.
 const h=codeHost(t,cwd,`if(a[0]==='send'&&flag('--request-id').includes('-fence-wait-')&&!fs.existsSync(__dirname+'/accept')){fs.writeSync(2,'tt: hub: 409 conflict: request id reused with different data\\n');process.exit(1);}`);
 h.setJobs([verifiedJob("rel_a"),verifiedJob("rel_b")]);let polls=0;
 const err=await heldRun(t,h,4,async(c,adapter)=>{if(++polls===3)writeFileSync(join(h.home,"accept"),"");adapter.matrixWait=WAKE_WAIT;return {outcome:"waiting_matrix"};});
 assert.equal(polls,4,"the run of four polls ended without throwing");assert.equal(err,"Fence wait notice not posted; the next poll retries.\n".repeat(2));
 // Each poll goes on after the refused send: the matrix notice of the first, and a claim in every one.
 const calls=h.calls().map(c=>c.argv),steps=calls.filter(a=>a[1]==="claim"||a[0]==="send").map(a=>a[1]==="claim"?"claim":argvField(a,"--request-id").includes("-fence-wait-")?"fence":"matrix");
 assert.deepEqual(steps,["claim","fence","matrix","claim","fence","claim","fence","claim"]);
 const fence=noticeArgv(h).filter(a=>argvField(a,"--request-id").includes("-fence-wait-"));assert.equal(fence.length,3);
 for(const a of fence){assert.equal(argvField(a,"--request-id"),"rel_a-fence-wait-rel_b-waiting_matrix");assert.deepEqual(wakeRefs(a),["wake=routine"]);assert.deepEqual(a,fence[0]);}
 assert.deepEqual(noticeArgv(h).map(a=>argvField(a,"--subject")).filter(s=>s!=="A release job is waiting behind a held project fence"),["A release job is waiting for the verification host"]);
 // One private record of the refused call, counted twice.
 const kept=JSON.parse(readFileSync(join(h.home,"cli-failures.json"),"utf8")).failures;assert.equal(kept.length,1);
 assert.deepEqual(kept[0].argv,[h.tt,...fence[0]]);assert.equal(kept[0].exitCode,1);assert.equal(kept[0].count,2);assert.match(kept[0].stderr,/request id reused with different data/);
});
