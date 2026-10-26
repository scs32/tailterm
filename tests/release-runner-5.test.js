// Go-licenses provisioning, matrix-run proof, failed receipts, target
// selection, refusal reasons and the release quiet window, split from
// tests/release-runner-2.test.js, and release batch unions, split from
// tests/release-runner-4.test.js (wi_f60103c0670388c8, order #44897), so
// node --test runs them beside the rest of those files.
import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,readdirSync,rmSync,renameSync} from "node:fs";
import {tmpdir} from "node:os";
import {join,dirname} from "node:path";
import {execFileSync,spawn,spawnSync} from "node:child_process";
import {runRelease,liveCheck,runnableJob,HostAdapter,cliRetry,serveDeployment,hostLockNames,receiptReason,probeDiagnostics,withoutDiagnostics,sameReceipt,validateNativeRelease,startMatch,reconcileReceipts,MATRIX_PREREQUISITES,MATRIX_HOST_WAIT_MS,MATRIX_LAUNCH_GRACE_MS,MATRIX_STOP_GRACE_MS,MATRIX_DEADLINE_SLACK_MS,provisionPrerequisites,matrixRunTimeout,matrixPriority,matrixWaitNotice,matrixHeldNotice,fenceWaitNotice,matrixRunUnsettled,batchMismatch,batchPlanCovers} from "../scripts/release-runner.mjs";
import {acquireHostLock,readJournal,updateHostState,pidGone,groupGone,RUN_TIMEOUT_GRACE_MS,DEFAULT_HOLDER_CAP_MS} from "../scripts/verify-matrix-host-lock.mjs";
import {planRunTimeout,provisionWorktreePrerequisites} from "../scripts/verify-matrix.mjs";
import {createHash} from "node:crypto";
import {targetsForPaths} from "../scripts/release-targets.mjs";
import {releaseReply,releaseReplySource,readHostState,git,fixture,change,job,fake,config,hash,REPO,recorded,C,remoteHead,PAIRED,pairedRelease,RUNNER,ignorePrerequisites,placePrerequisites,workedPlan,exitedPid,attemptDir,provisionFixture,until,ended,idle,lockHost,holderEntry,otherHolder,reasonIs,releaseHost,tailosAdapter,MATRIX_A,MATRIX_B,uncoveredReason,changedMatrixHost,matrixChangeRelease,heldRun,race,covers,stale,claimed,hubJob,journalAt,batchConfig,batchJournal,matrixRepo,matrixFake,hostPlan,checkIds} from "./release-runner-fixtures.mjs";
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
// wi_f8650bcc961b78bb: the source's .build/go-licenses tree, keyed by its own
// copy of the module list, and every file under a tree as path -> bytes.
const goLicenses=(from,key=readFileSync(join(from,".build/go-modules.txt")))=>{const dir=join(from,".build/go-licenses");mkdirSync(join(dir,"modules/example.com/a"),{recursive:true});writeFileSync(join(dir,"go-modules.txt"),key);writeFileSync(join(dir,"modules/example.com/a/LICENSE"),"licence a");writeFileSync(join(dir,"GOROOT-LICENSE"),"licence go");};
const treeOf=dir=>{const out={};const walk=rel=>{for(const name of readdirSync(join(dir,rel))){const r=rel?rel+"/"+name:name;if(statSync(join(dir,r)).isDirectory())walk(r);else out[r]=readFileSync(join(dir,r)).toString("base64");}};walk("");return out;};
const provisioned=(p,lines=[])=>({receipt:provisionPrerequisites(p.cwd,{from:p.from,run:p.run,print:l=>lines.push(l)}),lines});
test("g1 g3 a copied module list brings the source's go-licenses copies when their key matches, and a second run changes nothing",()=>{
 const p=provisionFixture();goLicenses(p.from);
 const {receipt,lines}=provisioned(p);
 assert.deepEqual(lines,[".build/go-licenses copied: its go-modules.txt matches the copied module list"]);
 assert.deepEqual(treeOf(join(p.cwd,".build/go-licenses")),treeOf(join(p.from,".build/go-licenses")));assert.equal(Object.keys(treeOf(join(p.cwd,".build/go-licenses"))).length,3);
 assert.deepEqual(readdirSync(join(p.cwd,".build")).sort(),["go-licenses","go-modules.txt","speech-fixture.wav","test.wasm"],"no temporary copy left");
 assert.deepEqual(receipt.prerequisites.map(x=>[x.path,x.action]),MATRIX_PREREQUISITES.map((x,i)=>[x,i?"copied":"installed"]));
 assert.deepEqual(Object.keys(receipt).sort(),["prerequisites","version"]);for(const x of receipt.prerequisites)assert.deepEqual(Object.keys(x).sort(),x.action==="copied"?["action","path","sha256","sourceCommit"]:["action","path","sha256"]);
 assert.deepEqual(receipt.prerequisites.map(x=>x.sourceCommit),MATRIX_PREREQUISITES.map((x,i)=>i?p.commit:undefined),"copied entries name the source commit");
 assert.equal(git(p.cwd,"status","--porcelain"),"");
 const before=treeOf(join(p.cwd,".build"));writeFileSync(join(p.from,".build/go-licenses/modules/example.com/a/LICENSE"),"changed in the source");
 const again=provisioned(p);
 assert.deepEqual(again.lines,[]);assert.ok(again.receipt.prerequisites.every(x=>x.action==="present"));
 assert.deepEqual(treeOf(join(p.cwd,".build")),before,"the second run changes nothing");
});
test("g1 g3 a mismatched key or a missing tree copies none of it and names the reason",()=>{
 const p=provisionFixture();goLicenses(p.from,"fixture another build's list");
 const mismatched=provisioned(p);
 assert.deepEqual(mismatched.lines,[".build/go-licenses not copied: its go-modules.txt differs from the copied module list; licence texts come from the module directories the list names"]);
 assert.ok(!existsSync(join(p.cwd,".build/go-licenses")));assert.equal(mismatched.receipt.prerequisites.at(-1).action,"copied");
 const q=provisionFixture(),missing=provisioned(q);
 assert.deepEqual(missing.lines,[".build/go-licenses not copied: the source has no .build/go-licenses/go-modules.txt; licence texts come from the module directories the list names"]);
 assert.ok(!existsSync(join(q.cwd,".build/go-licenses")));
 const k=provisionFixture();goLicenses(k.from);rmSync(join(k.from,".build/go-licenses/go-modules.txt"));
 assert.match(provisioned(k).lines[0],/not copied: the source has no \.build\/go-licenses\/go-modules\.txt/);assert.ok(!existsSync(join(k.cwd,".build/go-licenses")),"no key, no tree");
 for(const x of [p,q,k])assert.deepEqual(readdirSync(join(x.cwd,".build")).sort(),["go-modules.txt","speech-fixture.wav","test.wasm"]);
 const c=provisionFixture();mkdirSync(join(c.cwd,"node_modules"));writeFileSync(join(c.cwd,"node_modules/.package-lock.json"),"{}");goLicenses(c.from,"fixture another build's list");
 const r=spawnSync(process.execPath,[RUNNER,"--provision-prerequisites","--from",c.from],{cwd:c.cwd,encoding:"utf8"});
 assert.equal(r.status,0);assert.equal(r.stderr,".build/go-licenses not copied: its go-modules.txt differs from the copied module list; licence texts come from the module directories the list names\n");
 const out=JSON.parse(r.stdout);assert.deepEqual(Object.keys(out).sort(),["prerequisites","version"]);assert.deepEqual(out.prerequisites.map(x=>x.action),["present","copied","copied","copied","copied"]);
 for(const x of out.prerequisites)assert.deepEqual([Object.keys(x).sort(),x.sourceCommit],x.action==="copied"?[["action","path","sha256","sourceCommit"],c.commit]:[["action","path","sha256"],undefined]);
});
test("g1 g3 an existing target go-licenses tree is left untouched",()=>{
 const p=provisionFixture();goLicenses(p.from);mkdirSync(join(p.cwd,".build/go-licenses"),{recursive:true});writeFileSync(join(p.cwd,".build/go-licenses/go-modules.txt"),"local list");
 const {receipt,lines}=provisioned(p);
 assert.deepEqual(lines,[".build/go-licenses not copied: the checkout already has one; licence texts come from the module directories the list names"]);
 assert.deepEqual(treeOf(join(p.cwd,".build/go-licenses")),{"go-modules.txt":Buffer.from("local list").toString("base64")});
 assert.equal(receipt.prerequisites.at(-1).action,"copied");assert.equal(git(p.cwd,"status","--porcelain"),"");
 // A module list already present is not copied, so its licence tree is not looked at.
 const q=provisionFixture();goLicenses(q.from);mkdirSync(join(q.cwd,".build"));writeFileSync(join(q.cwd,".build/go-modules.txt"),"local list");
 assert.deepEqual(provisioned(q).lines,[]);assert.ok(!existsSync(join(q.cwd,".build/go-licenses")));
});
test("g1 a matrix run in a linked worktree prints whether the go-licenses copies came",()=>{
 const checks=[{id:"fixture-browser",argv:["node","fixture.mjs"],environment:{TEST_BROWSER:"both"}}];
 for(const [key,line] of [[undefined,"matrix run: .build/go-licenses copied: its go-modules.txt matches the copied module list"],["fixture another build's list","matrix run: .build/go-licenses not copied: its go-modules.txt differs from the copied module list; licence texts come from the module directories the list names"]]){
  const p=provisionFixture();placePrerequisites(p.cwd);goLicenses(p.cwd,key);
  const wt=join(mkdtempSync(join(tmpdir(),"provision-linked-")),"wt");git(p.cwd,"worktree","add","-q","--detach",wt);
  const printed=[];provisionWorktreePrerequisites(checks,wt,{installPrerequisites:p.run,hostLock:{print:l=>printed.push(l)}});
  assert.equal(printed.at(-1),line);assert.equal(printed.length,7);assert.equal(existsSync(join(wt,".build/go-licenses")),key===undefined);
  const head=git(p.cwd,"rev-parse","HEAD");assert.match(printed[1],/^matrix run: prerequisite node_modules\/\.package-lock\.json installed [0-9a-f]{64}$/);
  for(const l of printed.slice(2,6))assert.match(l,new RegExp("^matrix run: prerequisite \\S+ copied [0-9a-f]{64} from "+head+"$"));
  assert.equal(git(wt,"status","--porcelain"),"");
 }
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
  writeFileSync(join(e.dir,"host-lock.json"),JSON.stringify({outcome,pid:exitedPid(),requestedAt:new Date().toISOString()}));
  await assert.rejects(e.adapter.verifyIntegrated(e.integrated),reasonIs(reason));assert.equal(e.run().state,"ended");assert.equal(e.starts(),1);
 }
 // A pid in the lock file that began after it joined is not proof of our run.
 const reused=launch(t);assert.equal(await reused.adapter.verifyIntegrated(reused.integrated),false);
 assert(updateHostState(reused.path,state=>{state.waiters.push({...holderEntry(reused.child.pid,reused.dir,{requestedAt:"2020-01-01T00:00:00.000Z"}),grantSeqAtRequest:0,overtakenBy:0});}).done);
 assert.equal(await reused.adapter.verifyIntegrated(reused.integrated),false);assert.deepEqual([reused.run().state,reused.run().processStartedAt],["started",null]);
});
// wi_583c784aed186b11 v1: an unconfirmed launch with no child or lock entry
// ends from its sidecar only once the sidecar's own pid is gone or reused; a
// pid begun by its join time is the run, adopted; no pid or no readable start
// time is held by name.
test("a5 (iii) v1 a starting record's sidecar ends the job only with exit proof from its own pid",async t=>{
 const launch=(t,sidecar)=>{const h=lockHost(t);let clock=1000000;h.adapter.now=()=>clock;h.tick=ms=>{clock+=ms;};
  h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return exitedPid();};
  const save=h.adapter.saveRun.bind(h.adapter);let saves=0;h.adapter.saveRun=(dir,run)=>{if(++saves===2)throw new Error("lost");save(dir,run);};
  h.sidecar=async()=>{assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.run().state,"starting");writeFileSync(join(h.dir,"host-lock.json"),JSON.stringify({outcome:"released",...sidecar}));};
  return h;};
 const now=()=>new Date().toISOString();
 // Its pid is alive and began by the time it joined: the run, still running, is adopted and waited for.
 const live=idle(t);await new Promise(r=>live.once("spawn",r));
 const adopted=launch(t,{pid:live.pid,requestedAt:now()});await adopted.sidecar();
 assert.equal(await adopted.adapter.verifyIntegrated(adopted.integrated),false);
 assert.deepEqual([adopted.run().state,adopted.run().pid,adopted.adapter.matrixHeld],["started",live.pid,null]);assert.match(adopted.run().processStartedAt,ISO_START);
 assert.equal(await adopted.adapter.verifyIntegrated(adopted.integrated),false,"a live adopted run is waited for");assert.equal(adopted.run().state,"started");
 // Its pid is alive but began after it joined: reused, so the run is gone and ends by its outcome.
 const reused=launch(t,{pid:live.pid,requestedAt:"2020-01-01T00:00:00.000Z"});await reused.sidecar();
 await assert.rejects(reused.adapter.verifyIntegrated(reused.integrated),reasonIs("Integrated matrix run ended without a receipt"));assert.equal(reused.run().state,"ended");
 // Its pid is gone: today's ending.
 const gone=launch(t,{pid:exitedPid(),requestedAt:now()});await gone.sidecar();
 await assert.rejects(gone.adapter.verifyIntegrated(gone.integrated),reasonIs("Integrated matrix run ended without a receipt"));
 // No pid, an unreadable start time or an unreadable join time: held by name, never ended or signalled.
 for(const [sidecar,unreadable,reason,pid] of [[{},false,"matrix run sidecar names no pid",null],[{pid:"x",requestedAt:now()},false,"matrix run sidecar names no pid",null],
  [{pid:live.pid,requestedAt:now()},true,"no process-instance proof for the matrix run sidecar pid",live.pid],[{pid:live.pid,requestedAt:"soon"},false,"no process-instance proof for the matrix run sidecar pid",live.pid]]){
  const h=launch(t,sidecar);await h.sidecar();if(unreadable)h.adapter.processStartTime=()=>null;
  for(let poll=0;poll<2;poll++){h.tick(3600000);assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);}
  assert.deepEqual(h.adapter.matrixHeld,{attempt:h.integrated.integratedCommit+"-r0",pid,groups:[],reason});
  assert.equal(matrixHeldNotice({id:"rel_fixture"},h.adapter.matrixHeld).text.includes(reason),true,"the held notice accepts the reason");
  assert.equal(h.run().state,"starting");assert.deepEqual(h.signals,[]);
 }
 assert.equal(pidGone(live.pid),false,"never signalled");
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
  writeFileSync(join(dir,"host-lock.json"),JSON.stringify({pid:exitedPid(),requestedAt:new Date().toISOString(),...sidecar}));if(receipt)writeFileSync(join(dir,"receipt.json"),JSON.stringify(receipt));return h;};
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
// wi_583c784aed186b11 e1: a pid whose start time differs from the recorded
// one names another process, so the run is gone, before or past its deadline.
test("a10 (iii) e1 a reused pid with another start time counts as exited, so the run ends by the post-exit rules and nothing is signalled",async t=>{
 for(const late of [false,true]){
  const h=lockHost(t),other=idle(t);let clock=5000000;h.adapter.now=()=>clock;h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return other.pid;};
  h.adapter.processStartTime=()=>"2026-01-01T00:00:00.000Z";
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.run().processStartedAt,"2026-01-01T00:00:00.000Z");
  // The pid now belongs to an unrelated live process; the lock file still names it for this directory.
  delete h.adapter.processStartTime;assert.equal(startMatch("2026-01-01T00:00:00.000Z",h.adapter.processStartTime(other.pid)),"other");
  assert(updateHostState(h.path,state=>{state.holders=[holderEntry(other.pid,h.dir)];}).done);
  if(late)clock=h.deadline()+3600000;
  await assert.rejects(h.adapter.verifyIntegrated(h.integrated),reasonIs("Integrated matrix run ended without a receipt"));
  assert.deepEqual(h.signals,[]);assert.equal(pidGone(other.pid),false);assert.equal(h.adapter.matrixHeld,null);
  assert.deepEqual([h.run().state,h.run().reason,h.run().stopRequestedAt],["ended","no-receipt",undefined]);assert.deepEqual(h.run().snapshot.groups,[]);
 }
});
test("a10 (iii) e1 a live pid whose start time cannot be read now is held unsignalled past the deadline",async t=>{
 const h=lockHost(t),other=idle(t);let clock=5000000;h.adapter.now=()=>clock;h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return other.pid;};
 h.adapter.processStartTime=()=>"2026-01-01T00:00:00.000Z";
 assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);h.adapter.processStartTime=()=>null;
 assert(updateHostState(h.path,state=>{state.holders=[holderEntry(other.pid,h.dir)];}).done);
 clock=h.deadline()-1;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.adapter.matrixHeld,null,"before the deadline it waits");
 clock=h.deadline()+3600000;
 for(let poll=0;poll<3;poll++){clock+=MATRIX_STOP_GRACE_MS;assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);}
 assert.deepEqual(h.signals,[]);assert.equal(pidGone(other.pid),false);
 assert.deepEqual(h.adapter.matrixHeld,{attempt:h.integrated.integratedCommit+"-r0",pid:other.pid,groups:[],reason:"no process-instance proof for the matrix run pid"});
 assert.equal(h.run().state,"started");assert.equal(h.run().stopRequestedAt,undefined);
});
// The matrix run proof under the push group's rule: a run launched by a
// runner in another TZ and locale is proven and stopped at its deadline; a
// start time journaled before the fix is no proof, so the job is held.
test("a10 (iii) a run recorded under another TZ and locale is proven, and one recorded as rendered text is held unsignalled",async t=>{
 const env=otherEnvironments(t);
 for(const legacy of [false,true]){
  env.as("Pacific/Auckland","de_DE.UTF-8");
  const h=lockHost(t),other=idle(t);let clock=5000000;h.adapter.now=()=>clock;h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return other.pid;};
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.match(h.run().processStartedAt,ISO_START);
  if(legacy)writeFileSync(join(h.dir,"run.json"),JSON.stringify({...h.run(),processStartedAt:env.rendered(other.pid)}));
  env.as("America/Los_Angeles",undefined);
  assert(updateHostState(h.path,state=>{state.holders=[holderEntry(other.pid,h.dir)];}).done);
  clock=h.deadline();assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);
  if(legacy){
   assert.deepEqual(h.signals,[]);assert.equal(pidGone(other.pid),false);assert.equal(h.adapter.matrixHeld.reason,"no process-instance proof for the matrix run pid");
  }else{
   assert.deepEqual(h.signals,[[other.pid,"SIGTERM"]]);assert.equal(h.adapter.matrixHeld,null);assert.equal(h.run().stopRequestedAt,clock);
  }
 }
});
test("a5 (iii) lock-file adoption takes only a start time that compares, made before the pid joined",async t=>{
 for(const [started,ours] of [["2020-01-01T00:00:00.000Z",true],["Wed Jan  1 00:00:00 2020",false],[null,false]]){
  const h=lockHost(t),child=idle(t);h.adapter.startMatrixRun=(argv,dir)=>{h.calls.push({argv,dir});return child.pid;};
  const save=h.adapter.saveRun.bind(h.adapter);let saves=0;h.adapter.saveRun=(dir,run)=>{if(++saves===2)throw new Error("lost");save(dir,run);};
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.equal(h.run().state,"starting");
  h.adapter.processStartTime=()=>started;
  assert(updateHostState(h.path,state=>{state.waiters.push({...holderEntry(child.pid,h.dir),grantSeqAtRequest:0,overtakenBy:0});}).done);
  assert.equal(await h.adapter.verifyIntegrated(h.integrated),false);assert.deepEqual([h.run().state,h.run().processStartedAt],["started",ours?started:null]);
 }
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
 // wi_8afe404c4867dafc: a hub or tt older than the push diagnostics drops them.
 const bare={...old,outcome:"released",revert:undefined,targets:[],push:{remote:"origin",commit,outcome:"failed"}};delete bare.revert;
 const pushed={...bare,push:{...bare.push,timedOut:true,signal:"sigterm",elapsedMs:300004,reason:"push-timed-out",remoteCheck:"absent",stderrSHA256:"e".repeat(64)}},otherPush={...pushed,push:{...pushed.push,elapsedMs:1}};
 const released=(sent,saved)=>validateNativeRelease(prior,{...prior,state:"released",generation:3,receipt:saved},"finish",2,[],sent);
 const reconcileReleased=(sent,saved)=>{
  writeFileSync(join(home,"rel_fixture.json"),JSON.stringify({jobId:"rel_fixture",commit:prior.commit,agentId:prior.agentId,runId:prior.runId,phase:"receipt_pending",finishGeneration:2,receipt:sent}));
  reconcileReceipts({journalDirectory:home},[{...prior,state:"released",generation:3,receipt:saved}]);
  return JSON.parse(readFileSync(join(home,"rel_fixture.json"),"utf8")).phase;
 };
 assert.deepEqual(withoutDiagnostics(pushed),bare);assert.ok(sameReceipt(pushed,bare));assert.ok(!sameReceipt(bare,pushed));
 for(const [sent,saved] of [[pushed,pushed],[pushed,bare],[bare,bare]]){assert.equal(released(sent,saved).receipt,saved);assert.equal(reconcileReleased(sent,saved),"complete");}
 for(const [sent,saved] of [[pushed,otherPush],[bare,pushed]]){
  assert.throws(()=>released(sent,saved),/Native full receipt mismatch/);assert.throws(()=>reconcileReleased(sent,saved),/Saved release receipt does not match pending journal/);
 }
});
test("R5 the escalation names the TailOS waits and never echoes release.json content",async()=>{
 const cwd=mkdtempSync(join(tmpdir(),"escalate-tailos-")),calls=[];const adapter=new HostAdapter({cwd,journalDirectory:cwd},{id:"rel_fixture"});adapter.command=argv=>{calls.push(argv);return "";};
 await adapter.escalate({outcome:"rolled_back",probeWaits:[{target:"tailos",probe:"live",lastCommit:"c".repeat(40),waitedMs:90000},{target:"tailos",probe:"rollback",lastCommit:null,waitedMs:89600},{target:"tailos",probe:"rollback",lastCommit:"<b>SYNTHETIC_PRIVATE_TOKEN</b>",waitedMs:"90 s; rm"}]});
 const text=calls[0][calls[0].indexOf("--text")+1];
 assert.ok(text.includes(`TailOS live check last saw ${"c".repeat(40)} after 90 s.`));assert.ok(text.includes("TailOS rollback probe last saw no readable release.json after 90 s."));
 assert.ok(!text.includes("SYNTHETIC")&&!text.includes("90 s; rm"),text);
 // A live check that passed is left out; the rollback probe is still named.
 await adapter.escalate({outcome:"rolled_back",probeWaits:[{target:"tailos",probe:"live",lastCommit:"d".repeat(40),waitedMs:30000,passed:true},{target:"tailos",probe:"rollback",lastCommit:"e".repeat(40),waitedMs:4000}]});
 const passed=calls[1][calls[1].indexOf("--text")+1];
 assert.ok(!passed.includes("live check")&&!passed.includes("d".repeat(40)),passed);assert.ok(passed.includes(`TailOS rollback probe last saw ${"e".repeat(40)} after 4 s.`),passed);
});
test("p4 the escalation marks a live check that passed and not one that failed",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"hub/cmd/tt"),{recursive:true});change(f,"hub/cmd/tt/main.go","fixture");const j=job(f,change(f,"client/a.js","a")),a=fake();let escalation;
 const wait={lastCommit:"c".repeat(40),waitedMs:9000};
 a.check=async t=>t==="tailos"?"identity":true;a.probeWait=(t,kind)=>kind==="live"?wait:undefined;a.escalate=async d=>{escalation=d;a.calls.push("escalate");};
 await assert.rejects(runRelease(config(f,j),a),/inspect saved journal/);
 assert.deepEqual(escalation.probeWaits,[{target:"mini",probe:"live",...wait,passed:true},{target:"tailos",probe:"live",...wait}]);
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

// Refusal reason on the job (wi_9b18784bb4bffb14). The refuse carries the
// stable tag, sent as receipt reasons are, and the failed step and target.
test("f2 an unknown-path refusal sends its select step and tag with the refuse",async()=>{
 const n=newFolder(false),sent=[];n.a.refuse=async r=>{sent.push(r);n.a.calls.push("refuse");};
 await assert.rejects(runRelease(n.c,n.a),/refused/);
 assert.deepEqual(sent,[{step:"select",reason:NEW_REASON}]);assert.deepEqual(n.said,[{jobId:"rel_fixture",outcome:"refused",reason:NEW_REASON}],"the notice is unchanged");
 assert.equal(n.journal().refusalReason,NEW_REASON);
});
test("f2 a secret-looking tag is sent unclassified while the journal and notice keep it; a failure outside a step sends the tag alone",async()=>{
 // A new folder whose name holds a secret word (fake): its select tag is not a receipt reason.
 const TAG="Unknown release path: tools/tokenizer/a.js",f=fixture();mkdirSync(join(f.cwd,"tools/tokenizer"),{recursive:true});
 const j=job(f,change(f,"tools/tokenizer/a.js","a")),a=fake(),c=config(f,j),sent=[];let said;
 a.refuse=async r=>{sent.push(r);a.calls.push("refuse");};a.escalate=async d=>{said=d;a.calls.push("escalate");};
 await assert.rejects(runRelease(c,a),/refused/);
 assert.deepEqual(sent,[{step:"select",reason:"unclassified"}]);assert.deepEqual(a.calls,["refuse","escalate"]);
 const journal=journalAt(c);assert.equal(journal.phase,"refused");assert.equal(journal.refusalReason,TAG);assert.deepEqual(journal.failure,{step:"select",reason:TAG});assert.equal(said.reason,TAG);
 const g=fixture(),k=job(g,change(g,"client/a.js","a")),b=fake(),d=config(g,k),alone=[];
 b.verifyInputs=async()=>{throw releaseError("Release inputs request failed");};b.refuse=async r=>{alone.push(r);b.calls.push("refuse");};
 await assert.rejects(runRelease(d,b),/refused/);
 assert.deepEqual(alone,[{reason:"Release inputs request failed"}]);assert.equal(journalAt(d).failure,undefined);
});
// A CLI that answers refuse like this hub, or like an older hub or tt, a
// failing transport or a moved generation when the refuse carries a refusal.
function refuseTT(mode,hub=hubJob(2)){
 const home=mkdtempSync(join(tmpdir(),"release-refusal-")),tt=join(home,"tt"),log=join(home,"calls"),state=join(home,"job.json");
 writeFileSync(state,JSON.stringify(hub));
 writeFileSync(tt,"#!"+process.execPath+`
const fs=require('fs'),a=process.argv.slice(2),job=JSON.parse(fs.readFileSync(${JSON.stringify(state)},'utf8')),mode=${JSON.stringify(mode)};
fs.appendFileSync(${JSON.stringify(log)},JSON.stringify(a)+'\\n');
const refusal=a[1]==='refuse'&&a.includes('--reason');
if(mode==='old-hub'&&refusal){process.stderr.write('tt: hub: 400 invalid scope metadata request: unknown field "refusal"\\n');process.exit(1);}
if(mode==='invalid'&&refusal){process.stderr.write('tt: hub: 400 invalid\\n');process.exit(1);}
if(mode==='saved-bare'&&refusal){process.stderr.write('tt: hub: 409 conflict: release: retry changed\\n');process.exit(1);}
if(mode==='old-tt'&&a.includes('--target')){process.stderr.write('flag provided but not defined: -target\\nUsage of deployment refuse:\\n  -target string\\ntt: flag provided but not defined: -target\\n');process.exit(1);}
if(mode==='conflict'&&a[1]==='refuse'){process.stderr.write('tt: hub: 409 conflict: release: only an unpublished claim may refuse\\n');process.exit(1);}
if(mode==='other'&&a[1]==='refuse'){process.stderr.write('SYNTHETIC_PRIVATE_STDERR refuse failed\\n');process.exit(3);}
if(mode==='transport'&&refusal){process.stderr.write('tt: hub: 503 service unavailable\\n');process.exit(1);}
if(mode==='generation'&&refusal){process.stderr.write('tt: hub: 409 conflict: release: generation changed\\n');process.exit(1);}
if(a[1]==='refuse'){job.state='refused';job.generation++;fs.writeFileSync(${JSON.stringify(state)},JSON.stringify(job));}
console.log(JSON.stringify(job));`);chmodSync(tt,0o755);
 return {home,config:{cwd:home,journalDirectory:home,tt},calls:()=>readFileSync(log,"utf8").trim().split("\n").map(l=>JSON.parse(l))};
}
const BARE=["deployment","refuse","--job","rel_fixture","--generation","2","--request-id","rel_fixture-refuse-2"];
test("f2 the host adapter's refuse sends the refusal as flags, and the bare call without one",async()=>{
 for(const [refusal,extra] of [[{step:"prepare",target:"hub",reason:"python3 exit 1"},["--reason","python3 exit 1","--step","prepare","--target","hub"]],[{reason:"integration-conflict: 2 paths"},["--reason","integration-conflict: 2 paths"]],[undefined,[]]]){
  const t=refuseTT("new"),adapter=new HostAdapter(t.config,hubJob(2));
  await adapter.refuse(refusal);
  assert.deepEqual(t.calls(),[[...BARE,...extra]]);assert.equal(adapter.job.state,"refused");assert.equal(adapter.job.generation,3);
 }
});
test("f2 an older hub or tt still refuses: one bare refuse follows with the same job, generation and request ID",async()=>{
 const refusal={step:"prepare",target:"hub",reason:"python3 exit 1"};
 // An older hub, a refusal this hub holds invalid, an older tt, and a bare refuse saved under this request ID before.
 for(const mode of ["old-hub","invalid","old-tt","saved-bare"]){
  const t=refuseTT(mode),adapter=new HostAdapter(t.config,hubJob(2));
  await adapter.refuse(refusal);
  assert.deepEqual(t.calls(),[[...BARE,"--reason","python3 exit 1","--step","prepare","--target","hub"],BARE],mode);assert.equal(adapter.job.state,"refused",mode);
 }
 // Through a whole run: the integrate refusal of an older hub ends refused.
 const f=fixture(),mine={baseCommit:f.base,commit:change(f,"client/a.js","a"),plan:{commit:f.base}},t=refuseTT("old-hub",hubJob(2,mine)),adapter=new HostAdapter({...t.config,cwd:f.cwd},hubJob(2,mine));let said;adapter.escalate=async d=>{said=d;};
 const c={...config(f,adapter.job),journalPath:join(t.home,"rel_fixture.json")};
 await assert.rejects(runRelease(c,adapter),/Release refused before publication/);
 const refuses=t.calls().filter(a=>a[1]==="refuse");
 assert.deepEqual(refuses,[[...BARE,"--reason","Release binding mismatch","--step","integrate"],BARE]);
 assert.equal(adapter.job.state,"refused");const journal=journalAt(c);assert.equal(journal.phase,"refused");assert.equal(journal.refuseFailure,undefined);assert.equal(journal.refusalReason,"Release binding mismatch");assert.equal(said.reason,"Release binding mismatch");
});
test("f2 a transport failure, a moved generation or any other refuse failure sends no bare refuse",async t=>{
 const sleep=cliRetry.sleep;cliRetry.sleep=()=>{};t.after(()=>{cliRetry.sleep=sleep;});
 const refusal={step:"select",reason:NEW_REASON},sent=[...BARE,"--reason",NEW_REASON,"--step","select"];
 const transport=refuseTT("transport"),a=new HostAdapter(transport.config,hubJob(2));
 await assert.rejects(a.refuse(refusal),e=>e.transport===true);
 assert.deepEqual(transport.calls(),[sent,sent,sent],"the transport tries, never the bare form");assert.equal(a.job.state,"claimed");
 const moved=refuseTT("generation"),b=new HostAdapter(moved.config,hubJob(2));
 await assert.rejects(b.refuse(refusal),e=>e.generationChanged===true);
 assert.deepEqual(moved.calls(),[sent]);assert.equal(b.job.state,"claimed");
 // The hub's state refusal and a failure it does not name are thrown as they were.
 for(const mode of ["conflict","other"]){
  const other=refuseTT(mode),c=new HostAdapter(other.config,hubJob(2));
  await assert.rejects(c.refuse(refusal),e=>e.refusalRefused===undefined && e.transport===undefined,mode);
  assert.deepEqual(other.calls(),[sent],mode);assert.equal(c.job.state,"claimed",mode);
 }
});

// The block of a job that failed with no receipt (wi_816228d539a80c48): a
// CLI that answers block like this hub (the job's generation only; get prints
// the job), or like an older hub or tt, a failing transport or another
// refusal when the block carries a revert.
function blockTT(mode,hub=hubJob(2)){
 const home=mkdtempSync(join(tmpdir(),"release-block-")),tt=join(home,"tt"),log=join(home,"calls"),state=join(home,"job.json");
 writeFileSync(state,JSON.stringify(hub));
 writeFileSync(tt,"#!"+process.execPath+`
const fs=require('fs'),a=process.argv.slice(2),job=JSON.parse(fs.readFileSync(${JSON.stringify(state)},'utf8')),mode=${JSON.stringify(mode)};
fs.appendFileSync(${JSON.stringify(log)},JSON.stringify(a)+'\\n');
const flag=n=>a.includes(n)?a[a.indexOf(n)+1]:undefined,revert=a[1]==='block'&&a.includes('--revert-outcome');
if(a[1]==='get'){console.log(JSON.stringify(job));process.exit(0);}
if(mode==='old-hub'&&revert){process.stderr.write('tt: hub: 400 invalid scope metadata request: unknown field "revert"\\n');process.exit(1);}
if(mode==='invalid'&&revert){process.stderr.write('tt: hub: 400 invalid\\n');process.exit(1);}
if(mode==='saved-bare'&&revert){process.stderr.write('tt: hub: 409 conflict: release: retry changed\\n');process.exit(1);}
if(mode==='old-tt'&&revert){process.stderr.write('flag provided but not defined: -revert-outcome\\nUsage of deployment block:\\ntt: flag provided but not defined: -revert-outcome\\n');process.exit(1);}
if(mode==='transport'&&revert){process.stderr.write('tt: hub: 503 service unavailable\\n');process.exit(1);}
if(mode==='conflict'&&a[1]==='block'){process.stderr.write('tt: hub: 409 conflict: release: owned execution required\\n');process.exit(1);}
if(mode==='other'&&a[1]==='block'){process.stderr.write('SYNTHETIC_PRIVATE_STDERR block failed\\n');process.exit(3);}
if(a[1]==='block'){
 if(Number(flag('--generation'))!==job.generation){process.stderr.write('tt: hub: 409 conflict: release: generation changed\\n');process.exit(1);}
 job.state='blocked';job.generation++;
 if(revert)job.revert={outcome:flag('--revert-outcome'),...(flag('--revert-commit')?{commit:flag('--revert-commit')}:{}),...(flag('--revert-bug-request')?{bugRequestId:flag('--revert-bug-request')}:{})};
 fs.writeFileSync(${JSON.stringify(state)},JSON.stringify(job));
}
console.log(JSON.stringify(job));`);chmodSync(tt,0o755);
 return {home,config:{cwd:home,journalDirectory:home,tt},calls:()=>readFileSync(log,"utf8").trim().split("\n").map(l=>JSON.parse(l)),hub:()=>JSON.parse(readFileSync(state,"utf8"))};
}
const BLOCK=g=>["deployment","block","--job","rel_fixture","--generation",String(g),"--request-id",`rel_fixture-block-${g}`];
const REVERT_COMMIT="d".repeat(40),INTEGRATED="b".repeat(40);
test("rv1 the host adapter's block sends the revert as flags, and the bare call without one",async()=>{
 for(const [revert,extra] of [
  [{outcome:"committed",commit:REVERT_COMMIT,bugRequestId:"rel_fixture-rollback-bug"},["--revert-outcome","committed","--revert-commit",REVERT_COMMIT,"--revert-bug-request","rel_fixture-rollback-bug"]],
  [{outcome:"failed"},["--revert-outcome","failed"]],
  [undefined,[]]]){
  const t=blockTT("new"),adapter=new HostAdapter(t.config,hubJob(2));
  await adapter.block({integrated:INTEGRATED,...(revert?{revert}:{})});
  assert.deepEqual(t.calls(),[[...BLOCK(2),...extra]]);assert.equal(adapter.job.state,"blocked");assert.equal(adapter.job.generation,3);assert.deepEqual(t.hub().revert,revert);
 }
 // The call an older runner made, with no details at all, is the bare block.
 const t=blockTT("new"),adapter=new HostAdapter(t.config,hubJob(2));
 await adapter.block();assert.deepEqual(t.calls(),[BLOCK(2)]);assert.equal(adapter.job.state,"blocked");
});
test("rv2 an older hub or tt still blocks: one bare block follows with the same job, generation and request ID",async t=>{
 const revert={outcome:"committed",commit:REVERT_COMMIT},sent=[...BLOCK(2),"--revert-outcome","committed","--revert-commit",REVERT_COMMIT];
 // An older hub, a revert this hub holds invalid, an older tt, and a bare block saved under this request ID before.
 for(const mode of ["old-hub","invalid","old-tt","saved-bare"]){
  const b=blockTT(mode),adapter=new HostAdapter(b.config,hubJob(2));
  await adapter.block({integrated:INTEGRATED,revert});
  assert.deepEqual(b.calls(),[sent,BLOCK(2)],mode);assert.equal(adapter.job.state,"blocked",mode);assert.equal(b.hub().revert,undefined,mode);
 }
 const sleep=cliRetry.sleep;cliRetry.sleep=()=>{};t.after(()=>{cliRetry.sleep=sleep;});
 const transport=blockTT("transport"),a=new HostAdapter(transport.config,hubJob(2));
 await assert.rejects(a.block({integrated:INTEGRATED,revert}),e=>e.transport===true);
 assert.deepEqual(transport.calls(),[sent,sent,sent],"the transport tries, never the bare form");assert.equal(a.job.state,"claimed");
 // The hub's state refusal and a failure it does not name are thrown as they were.
 for(const mode of ["conflict","other"]){
  const other=blockTT(mode),c=new HostAdapter(other.config,hubJob(2));
  await assert.rejects(c.block({integrated:INTEGRATED,revert}),e=>e.refusalRefused===undefined && e.transport===undefined,mode);
  assert.deepEqual(other.calls(),[sent],mode);assert.equal(c.job.state,"claimed",mode);
 }
});
test("rv3 a block whose merged reply was lost re-reads the job once and blocks at the hub's generation",async()=>{
 const revert={outcome:"committed",commit:REVERT_COMMIT,bugRequestId:"rel_fixture-rollback-bug"},flags=["--revert-outcome","committed","--revert-commit",REVERT_COMMIT,"--revert-bug-request","rel_fixture-rollback-bug"];
 const merged=hubJob(3,{state:"merged",published:true,integratedCommit:INTEGRATED});
 const t=blockTT("new",merged),adapter=new HostAdapter(t.config,hubJob(2));
 await adapter.block({integrated:INTEGRATED,revert});
 assert.deepEqual(t.calls(),[[...BLOCK(2),...flags],["deployment","get","--job","rel_fixture"],[...BLOCK(3),...flags]]);
 assert.equal(adapter.job.state,"blocked");assert.equal(adapter.job.generation,4);assert.deepEqual(t.hub().revert,revert);
 // Another run, another integrated commit, a job still claimed, or no
 // integrated commit to compare: nothing is adopted and the refusal is thrown.
 for(const [label,hub,integrated] of [
  ["another run",{...merged,runId:"run_other"},INTEGRATED],
  ["another commit",{...merged,integratedCommit:"e".repeat(40)},INTEGRATED],
  ["still claimed",hubJob(3),INTEGRATED],
  ["unpublished",{...merged,published:undefined},INTEGRATED],
  ["no integrated commit",merged,undefined]]){
  const n=blockTT("new",hub),a=new HostAdapter(n.config,hubJob(2));
  await assert.rejects(a.block({integrated,revert}),e=>e.generationChanged===true,label);
  assert.deepEqual(n.calls(),[[...BLOCK(2),...flags],["deployment","get","--job","rel_fixture"]],label);assert.equal(a.job.generation,2,label);assert.equal(n.hub().state,hub.state,label);
 }
});
test("rv4 a run that fails after publication with no receipt sends its journaled revert on block",async()=>{
 // The hub never saw merged (claimed at 2), or saved it and the reply was lost (merged at 3).
 for(const saved of [false,true]){
  const f=fixture(),j=job(f,change(f,"client/a.js","a")),a=fake(),c=config(f,j);
  a.merged=async()=>{throw new Error("merged response lost");};
  const t=blockTT("new",saved?hubJob(3,{state:"merged",published:true,integratedCommit:j.commit}):hubJob(2)),host=new HostAdapter(t.config,hubJob(2));
  a.block=async d=>{a.calls.push("block");return host.block(d);};
  await assert.rejects(runRelease(c,a),/inspect saved journal/);
  assert.deepEqual(a.calls,["escalate","bug","block"]);
  const journal=journalAt(c);assert.equal(journal.revert.outcome,"committed");assert.match(journal.revert.commit,/^[a-f0-9]{40}$/);assert.equal(journal.integrated,j.commit);
  const flags=["--revert-outcome","committed","--revert-commit",journal.revert.commit,"--revert-bug-request","rel_fixture-rollback-bug"];
  assert.deepEqual(t.calls(),saved?[[...BLOCK(2),...flags],["deployment","get","--job","rel_fixture"],[...BLOCK(3),...flags]]:[[...BLOCK(2),...flags]],String(saved));
  assert.equal(host.job.state,"blocked");assert.deepEqual(t.hub().revert,{outcome:"committed",commit:journal.revert.commit,bugRequestId:"rel_fixture-rollback-bug"});
 }
});

// The runner check of the release quiet window (wi_3427bf81094d2225): on every
// pass, after selection and the integrated matrix and before the inputs are
// bound, a window that is open over a selected target, or unreadable while any
// target is selected, holds the claim in waiting_window with no effect.
import {releaseWindowStatus,parseReleaseWindow,windowWaitNotice,unclaimableJob,WAKE} from "../scripts/release-runner.mjs";
import {codeHost,verifiedJob} from "./release-runner-fixtures.mjs";
const UNTIL="2026-10-21T07:00:00-07:00",NIGHT={zone:"America/Los_Angeles",start:"22:00",end:"07:00",targets:["hub","bridge","mini"],reason:"night"};
const OPEN={...NIGHT,open:true,until:UNTIL},CLOSED={...NIGHT,open:false};
// A fake adapter whose window reads, inputs requests and matrix imports are counted.
function windowed(window){
 const a=fake();a.reads=0;a.inputs=0;a.matrix=0;a.window=window;
 a.releaseWindow=async()=>{a.reads++;if(a.window instanceof Error)throw a.window;return typeof a.window==="function"?a.window():a.window;};
 a.verifyInputs=async()=>{a.inputs++;return a.inputsReady!==false;};
 a.verifyIntegrated=async()=>{a.matrix++;return a.matrixReady!==false;};
 return a;
}
// A docs-only job; then an owner commit no job records lands on tasks-hub after its base.
function nightFixture(paths=["hub/cmd/tt/x.go"]){
 const f=fixture();mkdirSync(join(f.cwd,"docs"));const j=job(f,change(f,"docs/note.md","note"));
 git(f.cwd,"checkout","tasks-hub");for(const p of paths){mkdirSync(join(f.cwd,dirname(p)),{recursive:true});writeFileSync(join(f.cwd,p),p);}
 const owner=change(f,paths[0],"owner");return {f,j,owner};
}
const journalOfWindow=c=>JSON.parse(readFileSync(c.journalPath,"utf8"));
const noEffect=a=>assert.deepEqual(a.calls,[]);
test("window a1 a docs-only job carrying an unrecorded hub commit waits inside the window with nothing bound, published or deployed",async()=>{
 const {f,j,owner}=nightFixture(),a=windowed(OPEN),c=config(f,j);
 const r=await runRelease(c,a);
 assert.deepEqual(r,{jobId:j.id,outcome:"waiting_window",window:{covered:["mini"],until:UNTIL,zone:"America/Los_Angeles",start:"22:00",end:"07:00"}});
 assert.equal(git(f.cwd,"rev-parse","tasks-hub"),owner);assert.equal(remoteHead(f),f.base);
 noEffect(a);assert.equal(a.inputs,0);assert.equal(a.reads,1);
 const journal=journalOfWindow(c);assert.equal(journal.phase,"waiting_window");assert.deepEqual(journal.effects,[]);assert.notEqual(journal.published,true);
 assert.deepEqual(journal.windowWait.covered,["mini"]);assert.equal(journal.windowWait.until,UNTIL);assert.equal(journal.integrated,git(f.cwd,"rev-parse","HEAD"));
 // Only the selected targets the window names are covered.
 const mixed=nightFixture(["hub/cmd/tt/x.go","client/x.js"]),m=windowed(OPEN);
 assert.deepEqual((await runRelease(config(mixed.f,mixed.j),m)).window.covered,["mini"]);noEffect(m);
});
test("window a2 a selection with no covered target releases in the window, and an empty one never reads it",async()=>{
 const f=fixture();mkdirSync(join(f.cwd,"docs"));const j=job(f,change(f,"docs/note.md","note")),a=windowed(OPEN);
 const r=await runRelease(config(f,j),a);assert.equal(r.outcome,"released");assert.deepEqual(r.targets,[]);assert.equal(a.reads,0);assert.deepEqual(a.calls,["merged","finish"]);
 const g=fixture(),k=job(g,change(g,"client/a.js","a")),b=windowed(OPEN);
 const s=await runRelease(config(g,k),b);assert.equal(s.outcome,"released");assert.deepEqual(b.calls,["merged","deploy:tailos","finish"]);assert.equal(b.reads,1);
 // A window of tailos alone does not cover a selected mini.
 const {f:h,j:l}=nightFixture(),d=windowed({...OPEN,targets:["tailos"]});
 assert.equal((await runRelease(config(h,l),d)).outcome,"released");assert.deepEqual(d.calls,["merged","deploy:mini","finish"]);
});
test("window a3 an unreadable window covers every selected target with its named cause and refuses nothing",async()=>{
 const cases=[
  [Object.assign(releaseError("tt deployment window connection refused, 3 tries"),{transport:true}),"tt deployment window connection refused, 3 tries"],
  [releaseError("tt deployment window exit 2"),"tt deployment window exit 2"],
  [new Error("SYNTHETIC_PRIVATE_TOKEN"),"unclassified"],
  [{open:"yes"},"Release window unreadable"],
  [{...OPEN,until:"tomorrow"},"Release window unreadable"],
  [{...OPEN,targets:["hub","nas"]},"Release window unreadable"],
 ];
 for(const [window,cause] of cases){
  const {f,j}=nightFixture(["hub/cmd/tt/x.go","client/x.js"]),a=windowed(window),c=config(f,j);
  const r=await runRelease(c,a);
  assert.equal(r.outcome,"waiting_window",cause);assert.deepEqual(r.window,{covered:["mini","tailos"],cause},cause);
  noEffect(a);assert.equal(a.inputs,0,cause);assert.ok(!readFileSync(c.journalPath,"utf8").includes("SYNTHETIC_PRIVATE_TOKEN"));
 }
 // The same unreadable window with a docs-only selection releases, unread.
 const f=fixture();mkdirSync(join(f.cwd,"docs"));const j=job(f,change(f,"docs/note.md","note")),a=windowed(releaseError("tt deployment window exit 2"));
 assert.equal((await runRelease(config(f,j),a)).outcome,"released");assert.equal(a.reads,0);
});
test("window a4 outside the window the job releases, and a waiting job goes on at the same integrated commit once it closes",async()=>{
 for(const window of [CLOSED,null]){
  const {f,j}=nightFixture(),a=windowed(window),r=await runRelease(config(f,j),a);
  assert.equal(r.outcome,"released",JSON.stringify(window));assert.deepEqual(a.calls,["merged","deploy:mini","finish"]);assert.equal(a.inputs,1);
 }
 const {f,j}=nightFixture(),a=windowed(OPEN),c=config(f,j);
 assert.equal((await runRelease(c,a)).outcome,"waiting_window");const integrated=git(f.cwd,"rev-parse","HEAD");assert.equal(a.inputs,0);
 a.window=CLOSED;const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");assert.equal(r.commit,integrated);assert.equal(git(f.cwd,"rev-parse","tasks-hub"),integrated);
 assert.equal(a.inputs,1);assert.deepEqual(a.calls,["merged","deploy:mini","finish"]);assert.equal(journalOfWindow(c).windowWait,undefined);
});
test("window a5 a claim from before the window waits at every pass without a second inputs request, keeping its integrated run",async()=>{
 const {f,j}=nightFixture(),a=windowed(CLOSED),c=config(f,j);a.inputsReady=false;
 assert.equal((await runRelease(c,a)).outcome,"waiting_inputs");assert.equal(a.inputs,1);const integrated=journalOfWindow(c).integrated;
 a.window=OPEN;
 for(let pass=0;pass<2;pass++){
  assert.equal((await runRelease(c,a)).outcome,"waiting_window");assert.equal(a.inputs,1);noEffect(a);
  assert.equal(journalOfWindow(c).integrated,integrated);assert.equal(git(f.cwd,"rev-parse","HEAD"),integrated);
 }
 assert.equal(a.reads,3);
 a.window=CLOSED;a.inputsReady=true;const r=await runRelease(c,a);
 assert.equal(r.outcome,"released");assert.equal(r.commit,integrated);assert.equal(a.inputs,2);assert.deepEqual(a.calls,["merged","deploy:mini","finish"]);
 // A job waiting for its integrated matrix when the window opens still has it imported, then waits.
 const m=nightFixture(),b=windowed(CLOSED),d=config(m.f,m.j);b.matrixReady=false;
 assert.equal((await runRelease(d,b)).outcome,"waiting_matrix");assert.equal(b.reads,0);
 b.window=OPEN;b.matrixReady=true;assert.equal((await runRelease(d,b)).outcome,"waiting_window");assert.equal(b.matrix,2);assert.equal(b.inputs,0);noEffect(b);
});
test("window a6 effects that began before the window opened finish without another read",async()=>{
 const {f,j}=nightFixture(),a=windowed(CLOSED),deploy=a.deploy;a.deploy=async t=>{a.window=OPEN;return deploy(t);};
 const r=await runRelease(config(f,j),a);assert.equal(r.outcome,"released");assert.equal(a.reads,1);assert.deepEqual(a.calls,["merged","deploy:mini","finish"]);
 // A journal resumed in pushing finishes with the window open and never reads it.
 const g=nightFixture(),b=windowed(CLOSED),c=config(g.f,g.j);let finishes=0;b.finish=async()=>{if(++finishes===1)throw new Error("lost");b.calls.push("finish");};
 assert.equal((await runRelease(c,b)).outcome,"receipt_pending");const journal=JSON.parse(readFileSync(c.journalPath,"utf8"));
 journal.phase="pushing";delete journal.receipt.push;writeFileSync(c.journalPath,JSON.stringify(journal));git(g.f.origin,"update-ref","refs/heads/tasks-hub",g.f.base);
 b.window=OPEN;b.reads=0;const s=await runRelease(c,b);assert.equal(s.outcome,"released");assert.equal(s.push.outcome,"pushed");assert.equal(b.reads,0);
 assert.equal(b.calls.filter(x=>x==="deploy:mini").length,1);
});
test("window a7 the host read, its notice, the clean-stop and fence lists, and the daemon's report",async t=>{
 // HostAdapter.releaseWindow reads tt deployment window and checks its shape.
 const seen=[],host=out=>Object.assign(new HostAdapter({tt:"tt",cwd:"/"},{}),{command:argv=>{seen.push(argv);return out;}});
 assert.equal(await host("null\n").releaseWindow(),null);assert.deepEqual(seen[0],["tt","deployment","window"]);
 assert.deepEqual(await host(JSON.stringify({...CLOSED,setAt:"x",requestId:"k"})).releaseWindow(),{open:false,zone:"America/Los_Angeles",start:"22:00",end:"07:00",targets:["hub","bridge","mini"]});
 assert.deepEqual(await host(JSON.stringify(OPEN)).releaseWindow(),{open:true,until:UNTIL,zone:"America/Los_Angeles",start:"22:00",end:"07:00",targets:["hub","bridge","mini"]});
 for(const bad of ["not json","[]","{}",JSON.stringify({...OPEN,until:undefined}),JSON.stringify({...OPEN,targets:[]}),JSON.stringify({...OPEN,targets:["mini","mini"]}),JSON.stringify({...OPEN,zone:"a b"}),JSON.stringify({...OPEN,start:"2200"})])
  await assert.rejects(host(bad).releaseWindow(),e=>failureReason(e)==="Release window unreadable",bad);
 assert.equal(parseReleaseWindow("null"),null);assert.equal(releaseWindowStatus(null),null);
 // The notice: covered targets and until; one request id per window end; attention when unreadable.
 const w={covered:["mini"],until:UNTIL,zone:"America/Los_Angeles",start:"22:00",end:"07:00"},n=windowWaitNotice({id:"rel_night"},w);
 assert.equal(n.wake,WAKE.routine);assert.equal(n.subject,"A release job waits for the release quiet window");
 assert.match(n.text,/^Release rel_night waits for the release quiet window 22:00-07:00 America\/Los_Angeles, which covers its selected targets mini until 2026-10-21T07:00:00-07:00\. /);
 assert.equal(windowWaitNotice({id:"rel_night"},{...w}).requestId,n.requestId);assert.notEqual(windowWaitNotice({id:"rel_night"},{...w,until:"2026-10-22T07:00:00-07:00"}).requestId,n.requestId);
 assert.ok(n.requestId.startsWith("rel_night-window-wait-"));
 const u=windowWaitNotice({id:"rel_night"},{covered:["mini","tailos"],cause:"tt deployment window exit 2"});
 assert.equal(u.wake,WAKE.attention);assert.match(u.text,/could not be read \(tt deployment window exit 2\), so every selected target counts as covered: mini, tailos\./);
 assert.equal(windowWaitNotice({id:"rel_night"},{covered:[]}),null);assert.equal(windowWaitNotice({id:"rel_night"},{covered:["nas"],until:UNTIL}),null);
 // A clean stop may give back a waiting_window claim with no effects.
 const home=mkdtempSync(join(tmpdir(),"release-window-stop-"));t.after(()=>rmSync(home,{recursive:true,force:true}));
 const own={id:"rel_night",state:"claimed",agentId:"agt_night",runId:"run_night",commit:"c".repeat(40),generation:3},journal=extra=>writeFileSync(join(home,"rel_night.json"),JSON.stringify({version:1,jobId:own.id,commit:own.commit,agentId:own.agentId,runId:own.runId,phase:"waiting_window",effects:[],...extra}));
 const base={jobs:[own],details:new Map([[own.id,own]]),agent:own.agentId,run:own.runId,journalDirectory:home,lock:join(home,"absent.lock"),matrixChildren:0};
 journal();assert.equal(unclaimableJob(base),"rel_night");journal({effects:[{target:"mini"}]});assert.equal(unclaimableJob(base),null);journal({published:true});assert.equal(unclaimableJob(base),null);
 // The fence notice for a job queued behind it is routine.
 const fenceNotice=fenceWaitNotice([own,{id:"rel_next",state:"verified"}],own,"waiting_window",false,false);
 assert.equal(fenceNotice.wake,WAKE.routine);assert.match(fenceNotice.text,/is waiting for the release quiet window to end/);
 // One daemon poll that ends waiting for the window: one window notice and the owner_hold report.
 const cwd=mkdtempSync(join(tmpdir(),"release-window-daemon-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const h=codeHost(t,cwd,`if(a[1]==='progress'){console.log('{}');process.exit(0);}`);h.setJobs([verifiedJob("rel_night")]);
 const err=await h.poll(null,async c=>({jobId:c.job.id,outcome:"waiting_window",window:w}),{once:true,progress:true});
 assert.equal(err,"Release waits for the release quiet window; nothing is bound or published until it ends.\n");
 const sends=h.sends().filter(s=>s.subject==="A release job waits for the release quiet window");assert.equal(sends.length,1);assert.equal(sends[0].requestId,windowWaitNotice({id:"rel_night"},w).requestId);
 const sent=h.calls().find(c=>c.argv[0]==="send").argv;assert.ok(sent.includes("release-job=rel_night"));assert.ok(sent.includes("wake=routine"));
 assert.equal(h.calls().map(c=>c.argv.join(" ")).at(-1),"deployment progress --phase poll --job rel_night --wait owner_hold");
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
test("union6 a batch run refused only for the matrix home cleanup error records no attribution and names the cleanup error as its cause",async()=>{
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
 // Every check passed: no failed list, no unaffected list, the cleanup error as the cause.
 let r=await run({failedChecks:[],unnamed:0,homeCleanupError:true});
 assert.deepEqual(r.fallback,{batchId:r.batchId,...base,cause:"matrix-home-cleanup-error"});
 // A refusal that names or counts nothing records no attribution either.
 r=await run({failedChecks:[],unnamed:0});
 assert.deepEqual(r.fallback,{batchId:r.batchId,...base});
 // With failed checks beside the cleanup error, attribution is unchanged.
 r=await run({failedChecks:["go-vet"],unnamed:0,homeCleanupError:true});
 assert.deepEqual(r.fallback,{batchId:r.batchId,...base,failed:[{checkId:"go-vet",jobs:["rel_b2"]}],unaffected:["rel_b1","rel_b3"]});
 r=await run({failedChecks:[],unnamed:1,homeCleanupError:true});
 assert.deepEqual(r.fallback,{batchId:r.batchId,...base,failed:[],unaffected:["rel_b1","rel_b2","rel_b3"],unnamed:1});
 // The host adapter marks a receipt refused only for the cleanup error, without its text.
 const h=matrixHost({receipt:{environment:{VERIFICATION_HOME_CLEANUP_ERROR:"rm /private/home: busy"},checks:[{id:"go-vet",exitCode:0},{id:"npm-unit",exitCode:0}]}});
 await assert.rejects(h.verify(h.integrated),e=>failureReason(e)==="Integrated matrix receipt is not eligible" && JSON.stringify(e.failedChecks)==="[]" && e.unnamed===0 && e.homeCleanupError===true && !JSON.stringify(e).includes("busy"));
 assert.equal(h.sends().length,0);
 // A receipt without the cleanup error is not marked.
 const g=matrixHost({receipt:{environment:{},checks:[{id:"go-vet",exitCode:1}]}});
 await assert.rejects(g.verify(g.integrated),e=>JSON.stringify(e.failedChecks)==='["go-vet"]' && e.homeCleanupError===undefined);
});
