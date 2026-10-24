// Held-claim restarts, transport retries and waits, progress reports and
// stale list snapshots, split from tests/release-runner-3.test.js
// (wi_f60103c0670388c8, order #44897) so node --test runs them beside the
// rest of that file.
import test from "node:test";
import assert from "node:assert/strict";
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,chmodSync,existsSync,statSync,rmSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {runRelease,HostAdapter,serveDeployment,failureReason,cliFailureNotice,cliRetry,transportWait,transportWaitNotice} from "../scripts/release-runner.mjs";
import {RELEASE_READ_ATTEMPTS} from "../scripts/release-inputs.mjs";
import {git,fixture,change,job,fake,config,recorded,remoteHead,until,ended,idle,reasonIs,CODE_AGENT,CODE_RUN,SAFE_TEXT,codeRepo,codeHost,verifiedJob,codeGate,codeSends,HELD_SECRET,HELD_HANDLER,HELD_HELPER,heldRoster,sendOf,heldOwn,heldJournal,HELD_SUBJECT,stale,claimed} from "./release-runner-fixtures.mjs";
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
else if(a[1]==='window')console.log('null');
else if(['get','check'].includes(a[1]))console.log(JSON.stringify(job));
else if(a[1]==='merged'){job.state='merged';job.generation++;job.integratedCommit=flag('--commit');save();}
else if(a[1]==='refuse'){job.state='refused';job.generation++;save();}
else if(a[1]==='finish'){const r=JSON.parse(fs.readFileSync(flag('--file'),'utf8'));job.state=r.outcome;job.generation++;job.receipt=r;save();}
else process.exit(2);`);chmodSync(tt,0o755);
 const slept=[],sleep=cliRetry.sleep;cliRetry.sleep=ms=>{slept.push(ms);};t.after(()=>{cliRetry.sleep=sleep;});
 const saved=()=>JSON.parse(readFileSync(jobPath,"utf8"));
 // A poll's adapter: the real CLI paths, with the target steps stubbed.
 const adapter=()=>Object.assign(new HostAdapter({cwd:f.cwd,tt,journalDirectory:home},saved()),{releaseTools:async()=>{},prepare:async()=>({release:"fixture-release",artifactSHA256:"b".repeat(64)}),deploy:async()=>{},check:async()=>true,rollback:async()=>true,retain:async()=>{},jobInputs:()=>({targets:{}})});
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
 // q2: the other connection failures the Go client and the network print are transport errors too, each with its fixed cause.
 const dial='tt: Get "http://hub.invalid/x": dial tcp 192.0.2.1:18765: connect: ';
 for(const [stderr,cause] of [['tt: Post "http://hub.invalid/x": unexpected EOF',"unexpected EOF"],['tt: Get "http://hub.invalid/x": EOF',"connection closed: EOF"],
  ['tt: Post "http://hub.invalid/x": write tcp 192.0.2.1:1->192.0.2.2:2: write: broken pipe',"broken pipe"],[dial+"network is unreachable","network is unreachable"],[dial+"no route to host","no route to host"],[dial+"host is down","host is down"],
  [dial+"operation timed out","timed out: connect"],[dial+"connection timed out","timed out: connect"],['tt: Get "http://hub.invalid/x": dial tcp: lookup hub.invalid: no such host',"no such host"],
  ['tt: Get "http://hub.invalid/x": net/http: TLS handshake timeout',"timed out: TLS handshake"],['tt: Get "http://hub.invalid/x": http: server closed idle connection',"server closed idle connection"]])
  assert.deepEqual(tried([{op:"get",from:1,to:null,stderr}],get,stderr),[3,2,`tt deployment get ${cause}, 3 tries`],stderr);
 // The hub's own answer is final whatever its words: a refusal, a validation answer, a 500.
 for(const stderr of ["tt: hub: 409 conflict: release: generation changed","tt: hub: 422 validation: subject mentions a timeout","tt: hub: 400 bad request: context deadline exceeded is not a subject","tt: hub: 404 not found","tt: hub: 500 internal: database connection reset",
  "tt: hub: 409 conflict: broken pipe in subject","tt: hub: 422 validation: no route to host is not a reason","tt: hub: 500 internal: unexpected EOF","tt: hub: 400 bad request: network is unreachable","tt: hub: 409 conflict: no such host"])
  assert.deepEqual(tried([{op:"get",from:1,to:null,stderr}],get,stderr),[1,0,"tt deployment get exit 1"],stderr);
 // r2: with no hub status, a line that only says timeout or timed out is not a transport error and is made once; nor is an EOF not in the client's form (a local read).
 for(const stderr of ["tt: timeout","tt: timed out","tt: the operation timed out","tt: lock wait timeout exceeded","tt: Timeout","tt: client.timeout exceeded","tt: unexpected EOF","tt: EOF"])
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
// send:routine and send:attention fail only that notice's send, and
// refuse:routine makes the hub refuse the routine notice.
function waitHost(t){
 const cwd=mkdtempSync(join(tmpdir(),"release-transport-wait-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const sleep=cliRetry.sleep,slept=[];cliRetry.sleep=ms=>{slept.push(ms);};const clock=transportWait.now;let now=Date.parse("2026-10-13T18:00:00.000Z");transportWait.now=()=>now;t.after(()=>{cliRetry.sleep=sleep;transportWait.now=clock;});
 const faults=join(cwd,"faults.json");let failing=[];const fail=ops=>{failing=ops;writeFileSync(faults,JSON.stringify(ops));};fail([]);
 const h=codeHost(t,cwd,`const failing=JSON.parse(fs.readFileSync(${JSON.stringify(faults)},'utf8'));const notice=a[0]==='send'?{'A deployer CLI call failed':'routine',[${JSON.stringify(ATTENTION)}]:'attention'}[a[a.indexOf('--subject')+1]]:undefined;
  if(notice&&failing.includes('refuse:'+notice)){fs.writeSync(2,'tt: hub: 409 conflict: request id reused with other data\\n');process.exit(1);}
  if(failing.includes(a[0]==='send'?'send':a[1])||(notice&&failing.includes('send:'+notice))){fs.writeSync(2,'tt: Get "http://hub.invalid/'+${JSON.stringify(HELD_SECRET)}+'": dial tcp: connect: connection refused\\n');process.exit(1);}`);
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
 // q1: the owed routine notice is tried at that poll too, and stays owed.
 assert.equal(w.routine().length,3);assert.equal(w.wait().routine,false);
 // The hub answers again: the owed attention notice is posted once, identical to the earlier tries, and the wait ends.
 w.fail([]);w.advance(1);assert.equal(await w.poll(),ESCALATED_LINE);assert.equal(existsSync(w.file),false);
 const tries=w.attention();assert.equal(tries.length,3);assert.equal(new Set(tries.map(x=>x.requestId+"\n"+x.text)).size,1);assert.ok(tries[0].text.includes(`since ${began}`));
 // q1: so is the owed routine notice, identical to its earlier tries.
 const told=w.routine();assert.equal(told.length,4);assert.equal(new Set(told.map(x=>x.requestId+"\n"+x.text)).size,1);
 await w.poll();assert.equal(w.attention().length,3);assert.equal(w.routine().length,4);
 // A wait that ends inside the bound owes nothing.
 w.fail(["handler"]);await w.poll();w.fail([]);w.advance(3);assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),false);assert.equal(w.attention().length,3);
});
test("wait r3 q1 the poll that ends a wait tries each owed notice on its own and keeps the wait until neither is unreached",async t=>{
 // Opens a wait whose routine notice is owed; past the bound when due is set, with the attention notice owed too.
 const open=async(w,due)=>{w.h.setJobs([verifiedJob("rel_wait")]);w.fail(["handler","send"]);await w.poll();if(due){w.advance(11);await w.poll();}
  assert.equal(w.wait().routine,false);assert.equal(w.wait().escalated,false);assert.equal(Boolean(w.wait().due),due);w.advance(1);};
 await t.test("routine owed, attention not due",async t=>{
  const w=waitHost(t);await open(w,false);w.fail([]);
  assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),false);assert.equal(w.routine().length,2);assert.equal(w.attention().length,0);
  await w.poll();assert.equal(w.routine().length,2);
 });
 await t.test("only the routine send unreached",async t=>{
  const w=waitHost(t);await open(w,true);w.fail(["send:routine"]);
  assert.equal(await w.poll(),ESCALATED_LINE);assert.equal(existsSync(w.file),true);assert.equal(w.wait().escalated,"posted");assert.equal(w.wait().routine,false);assert.equal(w.routine().length,3);assert.equal(w.attention().length,2);
  w.fail([]);w.advance(1);assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),false);assert.equal(w.routine().length,4);assert.equal(w.attention().length,2);
 });
 await t.test("only the attention send unreached",async t=>{
  const w=waitHost(t);await open(w,true);w.fail(["send:attention"]);
  assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),true);assert.equal(w.wait().routine,true);assert.equal(w.wait().escalated,false);assert.equal(w.routine().length,3);assert.equal(w.attention().length,2);
  w.fail([]);w.advance(1);assert.equal(await w.poll(),ESCALATED_LINE);assert.equal(existsSync(w.file),false);assert.equal(w.routine().length,3);assert.equal(w.attention().length,3);
 });
 await t.test("a refused routine notice is taken",async t=>{
  const w=waitHost(t);await open(w,false);w.fail(["refuse:routine"]);
  assert.equal(await w.poll(),"");assert.equal(existsSync(w.file),false);assert.equal(w.routine().length,2);
  w.fail([]);await w.poll();assert.equal(w.routine().length,2);
 });
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

// The runner's progress reports for the hub's hung-runner check
// (wi_95b575f0454c7052). The fake hub accepts the command unless a test says
// otherwise; argv is every call as words.
const PROGRESS_OK=`if(a[1]==='progress'){console.log('{}');process.exit(0);}`,REFUSED='tt: Post "http://hub.invalid/x": dial tcp: connect: connection refused';
const argvOf=h=>h.calls().map(c=>c.argv.join(" ")),reports=h=>argvOf(h).filter(x=>x.startsWith("deployment progress"));
const reporting=(h,release)=>h.poll(null,release,{once:true,progress:true});
// Several polls of one daemon with reports on; every poll ends in one release call.
async function progressRun(t,h,polls,release){
 const saved=[process.env.TAILTERM_AGENT,process.env.TAILTERM_RUN],write=process.stderr.write;let err="",polled=0;
 process.env.TAILTERM_AGENT=CODE_AGENT;process.env.TAILTERM_RUN=CODE_RUN;process.stderr.write=(chunk,encoding,done=encoding)=>{err+=chunk;if(typeof done==="function")done();return true;};
 const controller=new AbortController();t.mock.timers.enable({apis:["setTimeout"]});
 const settle=async()=>{for(let i=0;i<20;i++)await new Promise(r=>setImmediate(r));};
 try{
  const serving=serveDeployment(h.config,{signal:controller.signal,progress:true,release:async(c,adapter)=>{const out=await release(c,adapter);polled++;return out;}});
  for(let n=1;n<=polls;n++){while(polled<n)await settle();await settle();if(n===polls)controller.abort();t.mock.timers.tick(30000);}
  await serving;
 }finally{t.mock.timers.reset();process.stderr.write=write;for(const [i,k] of ["TAILTERM_AGENT","TAILTERM_RUN"].entries()){if(saved[i]===undefined)delete process.env[k];else process.env[k]=saved[i];}}
 return err;
}
test("progress a13 a reporting poll sends a start report before its first read and a poll report after its last call, naming the wait it ended in",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-progress-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const host=extra=>codeHost(t,cwd,PROGRESS_OK+(extra||""));
 // Nothing to do: the two reports bracket the poll's reads.
 const idleHost=host();assert.equal(await reporting(idleHost),"");let calls=argvOf(idleHost);
 assert.equal(calls[0],"deployment progress --phase start");assert.equal(calls.at(-1),"deployment progress --phase poll");assert.ok(calls.length>2);assert.deepEqual(reports(idleHost),[calls[0],calls.at(-1)]);
 assert.ok(calls.slice(1,-1).every(x=>x.startsWith("deployment list ")));
 // A poll held by an unreadable job list still ends in its poll report.
 const heldHost=host(`if(a[1]==='list'){fs.writeSync(1,'not json');process.exit(0);}`);assert.match(await reporting(heldHost),/^Deployment poll held/);
 assert.deepEqual(reports(heldHost),["deployment progress --phase start","deployment progress --phase poll"]);assert.equal(argvOf(heldHost).at(-1),"deployment progress --phase poll");
 // A release that waits on its matrix run: the report names the job, the wait and the run's pid; the job's adapter reports too.
 const matrix=host();matrix.setJobs([verifiedJob("rel_matrix")]);let on=null;
 await reporting(matrix,async(c,adapter)=>{on=adapter.progressOn;adapter.matrixPid=4242;return {jobId:c.job.id,outcome:"waiting_matrix"};});
 assert.equal(on,true);assert.equal(argvOf(matrix).at(-1),"deployment progress --phase poll --job rel_matrix --wait matrix --pid 4242");
 const adopted=host();adopted.setJobs([verifiedJob("rel_matrix")]);await reporting(adopted,async c=>({jobId:c.job.id,outcome:"waiting_matrix"}));
 assert.equal(argvOf(adopted).at(-1),"deployment progress --phase poll --job rel_matrix --wait matrix");
 // The other waits a poll ends in, and a release that ended.
 const inputs=host();inputs.setJobs([verifiedJob("rel_inputs")]);await reporting(inputs,async c=>({jobId:c.job.id,outcome:"waiting_inputs"}));
 assert.equal(argvOf(inputs).at(-1),"deployment progress --phase poll --job rel_inputs --wait inputs");
 const done=host();done.setJobs([verifiedJob("rel_done")]);await reporting(done,async c=>({jobId:c.job.id,outcome:"released"}));
 assert.equal(argvOf(done).at(-1),"deployment progress --phase poll --job rel_done");
 const fence=host();fence.setJobs([{...verifiedJob("rel_holder"),state:"claimed",generation:2,agentId:"agt_0ther",runId:"run_0ther"},verifiedJob("rel_behind")]);await reporting(fence);
 assert.equal(argvOf(fence).at(-1),"deployment progress --phase poll --job rel_holder --wait fence");assert.equal(fence.claims().length,0);
 const hold=host();hold.setJobs([{...verifiedJob("rel_onhold"),state:"held"}]);await reporting(hold);
 assert.equal(argvOf(hold).at(-1),"deployment progress --phase poll --job rel_onhold --wait owner_hold");
 // Opt-in: a programmatic poll without the option and a bare adapter send nothing.
 const silent=host();silent.setJobs([verifiedJob("rel_quiet")]);let off=null;await silent.poll(null,async(c,adapter)=>{off=adapter.progressOn;adapter.progress({jobId:c.job.id,phase:"merged"});return {jobId:c.job.id,outcome:"waiting_matrix"};});
 assert.equal(off,false);assert.deepEqual(reports(silent),[]);
 new HostAdapter(silent.config,{}).progress({phase:"poll"});assert.deepEqual(reports(silent),[]);
});
test("progress a13 a release reports each phase and each step once, in order, with its job",async()=>{
 const f=fixture(),commit=change(f,"client/a.js","a"),seen=[],adapter={...fake(),progress:r=>{seen.push(r);}};
 const out=await runRelease(config(f,job(f,commit)),adapter);assert.equal(out.outcome,"released");
 assert.deepEqual(seen.map(r=>r.phase+(r.step?" "+r.step:"")),["prepared","prepared integrate","integrated","integrated select","merged","merged prepare","merged deploy","merged live-check","pushing","finishing","complete"]);
 assert.ok(seen.every(r=>r.jobId==="rel_fixture"));
 // A report that throws or rejects changes nothing, and an adapter without one is not told.
 for(const progress of [()=>{throw new Error("report failed");},()=>Promise.reject(new Error("report failed")),undefined]){
  const g=fixture(),c=change(g,"client/a.js","a"),quiet={...fake(),...(progress?{progress}:{})};
  assert.equal((await runRelease(config(g,job(g,c)),quiet)).outcome,"released");assert.deepEqual(quiet.calls,adapter.calls);
 }
 // The real adapter sends the report as one CLI call with the record's words.
 const cwd=mkdtempSync(join(tmpdir(),"release-progress-")),h=codeHost({after:fn=>fn},cwd,PROGRESS_OK);
 try{
  const real=Object.assign(new HostAdapter(h.config,{}),{progressOn:true});
  real.progress({jobId:"rel_fixture",phase:"merged",step:"live-check"});real.progress({phase:"poll",jobId:"rel_fixture",wait:{kind:"matrix",pid:7}});real.progress({phase:"poll",wait:{kind:"fence",pid:0}});real.progress({});
  assert.deepEqual(argvOf(h),["deployment progress --phase merged --job rel_fixture --step live-check","deployment progress --phase poll --job rel_fixture --wait matrix --pid 7","deployment progress --phase poll --wait fence"]);
 }finally{rmSync(cwd,{recursive:true,force:true});rmSync(h.home,{recursive:true,force:true});}
});
test("progress a14 a failed report is silent: a transport failure is one call, a rejection is followed by a withdraw until one succeeds, and nothing is recorded or posted",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-progress-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const waits=async c=>({jobId:c.job.id,outcome:"waiting_inputs"}),quiet=h=>{
  assert.equal(existsSync(join(h.home,"cli-failures.json")),false);assert.equal(existsSync(join(h.home,"transport-wait.json")),false);
  assert.deepEqual(h.calls().filter(c=>c.argv[0]==="send"&&!/waiting for its immutable inputs|Import immutable inputs/.test(c.argv.join(" "))).map(c=>c.argv.join(" ")),[]);
 };
 // What the same polls print with reports that succeed.
 const ok=codeHost(t,cwd,PROGRESS_OK);ok.setJobs([verifiedJob("rel_p")]);const plain=await progressRun(t,ok,2,waits),sends=ok.calls().filter(c=>c.argv[0]==="send").length;
 assert.equal(reports(ok).length,3);
 // The hub is unreached: every report is tried once and nothing is withdrawn.
 const down=codeHost(t,cwd,`if(a[1]==='progress'){fs.writeSync(2,${JSON.stringify(REFUSED)}+'\\n');process.exit(1);}`);down.setJobs([verifiedJob("rel_p")]);
 const slept=[],sleep=cliRetry.sleep;cliRetry.sleep=ms=>{slept.push(ms);};t.after(()=>{cliRetry.sleep=sleep;});
 assert.equal(await progressRun(t,down,2,waits),plain);
 assert.deepEqual(reports(down),["deployment progress --phase start","deployment progress --phase poll --job rel_p --wait inputs","deployment progress --phase poll --job rel_p --wait inputs"]);assert.deepEqual(slept,[]);quiet(down);
 assert.equal(down.calls().filter(c=>c.argv[0]==="send").length,sends);
 // q2: a report that fails in another transport phrase is one call too, and is not withdrawn.
 const unrouted=codeHost(t,cwd,`if(a[1]==='progress'){fs.writeSync(2,'tt: Post "http://hub.invalid/x": dial tcp 192.0.2.1:18765: connect: no route to host\\n');process.exit(1);}`);
 const lone=Object.assign(new HostAdapter(unrouted.config,{}),{progressOn:true});lone.progress({phase:"poll"});
 assert.deepEqual(reports(unrouted),["deployment progress --phase poll"]);assert.deepEqual(slept,[]);quiet(unrouted);
 // h2: this tt does not know the command (exit 2) and so refuses the withdraw too: it is sent again, in the form an older tt forwards, after each rejected report.
 const old=codeHost(t,cwd);old.setJobs([verifiedJob("rel_p")]);assert.equal(await progressRun(t,old,2,waits),plain);
 const withdraw="deployment progress --request-id progress-withdraw-"+CODE_RUN;
 assert.deepEqual(reports(old),["deployment progress --phase start",withdraw,"deployment progress --phase poll --job rel_p --wait inputs",withdraw,"deployment progress --phase poll --job rel_p --wait inputs",withdraw]);quiet(old);
 assert.equal(old.calls().filter(c=>c.argv[0]==="send").length,sends);
 // A tt from before the command, which forwards the withdraw form: one withdraw across the polls.
 const realOld=codeHost(t,cwd,`if(a[1]==='progress'){if(a.includes('--phase'))process.exit(2);console.log('{}');process.exit(0);}`);realOld.setJobs([verifiedJob("rel_p")]);
 assert.equal(await progressRun(t,realOld,2,waits),plain);
 assert.deepEqual(reports(realOld),["deployment progress --phase start",withdraw,"deployment progress --phase poll --job rel_p --wait inputs","deployment progress --phase poll --job rel_p --wait inputs"]);quiet(realOld);
 // A withdraw lost in transport is sent again after the next rejected report; once one succeeds, none follows until a report succeeds.
 const lost=join(cwd,"lost"),rejecting=join(cwd,"rejecting"),lossy=codeHost(t,cwd,`if(a[1]==='progress'){if(a.includes('--phase')&&fs.existsSync(${JSON.stringify(rejecting)}))process.exit(2);if(a.includes('--request-id')&&fs.existsSync(${JSON.stringify(lost)})){fs.writeSync(2,${JSON.stringify(REFUSED)}+'\\n');process.exit(1);}console.log('{}');process.exit(0);}`);
 const lossyAdapter=Object.assign(new HostAdapter(lossy.config,{}),{progressOn:true}),tell=()=>lossyAdapter.progress({phase:"poll"});
 writeFileSync(rejecting,"");writeFileSync(lost,"");tell();rmSync(lost);tell();tell();rmSync(rejecting);tell();writeFileSync(rejecting,"");tell();tell();
 assert.deepEqual(reports(lossy).map(x=>x.includes("--request-id")?"withdraw":"report"),["report","withdraw","report","withdraw","report","report","report","withdraw","report"]);
 assert.equal(lossyAdapter.progressState.withdrawn,true);assert.deepEqual(slept,[]);quiet(lossy);
 // Reports resume by themselves, and a rejection after a success is withdrawn again.
 const flaky=join(cwd,"reject"),turns=codeHost(t,cwd,`if(a[1]==='progress'){if(fs.existsSync(${JSON.stringify(flaky)})&&a.includes('--phase'))process.exit(2);console.log('{}');process.exit(0);}`);
 const adapter=Object.assign(new HostAdapter(turns.config,{}),{progressOn:true}),say=()=>adapter.progress({phase:"poll"});
 writeFileSync(flaky,"");say();say();rmSync(flaky);say();writeFileSync(flaky,"");say();say();
 assert.deepEqual(reports(turns).map(x=>x.includes("--request-id")?"withdraw":"report"),["report","withdraw","report","report","report","withdraw","report"]);quiet(turns);
});
test("claim a15 a claim that fails only in transport is quiet and opens the transport wait; a refused claim keeps the held-claim notice",async t=>{
 const w=waitHost(t),HELD_LINE="Release claim held; handler reconciliation required.\n",reason="tt deployment claim connection refused, 3 tries";
 w.h.setJobs([verifiedJob("rel_first"),verifiedJob("rel_second")]);const released=[],release=async c=>{released.push(c.job.id);return {jobId:c.job.id,outcome:"waiting_inputs"};};
 const claimsOf=()=>w.h.claims().map(c=>c.argv[c.argv.indexOf("--job")+1]),held=()=>w.h.calls().filter(c=>c.argv[0]==="send").map(sendOf).filter(x=>/skipped/.test(x.subject));
 // Every try of the first job's claim fails in transport.
 w.fail(["claim"]);const began=w.at(),err=await w.h.poll(null,release,{once:true,progress:true});
 assert.equal(err,"");assert.ok(!err.includes("reconciliation required"));assert.deepEqual(held(),[]);
 // The second job is not claimed in that poll, and nothing is released.
 assert.deepEqual(claimsOf(),["rel_first","rel_first","rel_first"]);assert.deepEqual(released,[]);
 // The transport wait opens with that job, as the transport fix built it.
 assert.deepEqual(w.wait(),{version:1,jobId:"rel_first",since:began,cause:reason,polls:1,routine:true,escalated:false,latest:reason,lastAt:began});assert.equal(w.routine().length,1);
 assert.deepEqual(w.routine()[0].refs,["release-job=rel_first","wake=routine"]);
 // The poll's last report names the transport wait (this fake rejects reports, so a withdraw follows each: h2).
 assert.equal(w.h.calls().filter(c=>c.argv[1]==="progress"&&c.argv.includes("--phase")).at(-1).argv.join(" "),"deployment progress --phase poll --job rel_first --wait transport");
 // Past the bound the wait escalates by itself; still no held claim.
 w.advance(10);assert.equal(await w.poll(),ESCALATED_LINE);assert.equal(w.attention().length,1);assert.deepEqual(held(),[]);assert.deepEqual(released,[]);
 // The fault clears: the next poll claims the first job and the wait ends.
 w.fail([]);assert.equal(await w.h.poll(null,release),"");assert.deepEqual(claimsOf().slice(-1),["rel_first"]);assert.deepEqual(released,["rel_first"]);assert.equal(existsSync(w.file),false);assert.deepEqual(held(),[]);
 // A claim the hub refuses is a held claim as before: the line, the notice, and the later job goes on.
 const cwd=mkdtempSync(join(tmpdir(),"release-held-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const h=codeHost(t,cwd,`if(a[1]==='claim'&&flag('--job')==='rel_first'){fs.writeSync(2,'tt: hub: 409 conflict: release: candidate changed since enqueue\\n');process.exit(1);}`+heldRoster);
 h.setJobs([verifiedJob("rel_first"),verifiedJob("rel_second")]);const went=[];
 assert.equal(await h.poll(null,async c=>{went.push(c.job.id);}),HELD_LINE);assert.deepEqual(went,["rel_second"]);
 const notices=h.calls().filter(c=>c.argv[0]==="send").map(sendOf);assert.deepEqual(notices.map(n=>n.to),[HELD_HANDLER,HELD_HELPER]);
 assert.ok(notices.every(n=>n.subject==="A release job is skipped because the hub refused its claim"));assert.equal(existsSync(join(h.home,"transport-wait.json")),false);
});
test("claim a16 a claim whose answer was lost is not sent again: the next poll runs the job as its own",async t=>{
 const cwd=mkdtempSync(join(tmpdir(),"release-lost-claim-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const sleep=cliRetry.sleep;cliRetry.sleep=()=>{};t.after(()=>{cliRetry.sleep=sleep;});
 // The fake hub records the claim, then fails the call in transport on every try.
 const jobs=join(cwd,"claimed.json");
 const h=codeHost(t,cwd,`if(a[1]==='claim'){fs.writeFileSync(${JSON.stringify(jobs)},JSON.stringify(jobs.map(j=>j.id===flag('--job')?{...j,state:'claimed',generation:j.generation+1,agentId:process.env.TAILTERM_AGENT,runId:process.env.TAILTERM_RUN}:j)));fs.writeSync(2,${JSON.stringify(REFUSED)}+'\\n');process.exit(1);}`);
 h.setJobs([verifiedJob("rel_lost"),verifiedJob("rel_later")]);const released=[],release=async c=>{released.push([c.job.id,c.job.state,c.job.generation,c.job.runId]);return {jobId:c.job.id,outcome:"released"};};
 assert.equal(await h.poll(null,release),"");assert.deepEqual(released,[]);
 assert.deepEqual(h.claims().map(c=>c.argv[c.argv.indexOf("--job")+1]),["rel_lost","rel_lost","rel_lost"]);
 assert.deepEqual(h.calls().filter(c=>c.argv[0]==="send").map(sendOf).filter(x=>/skipped/.test(x.subject)),[]);
 assert.equal(JSON.parse(readFileSync(join(h.home,"transport-wait.json"),"utf8")).jobId,"rel_lost");
 // The hub's list now shows the job claimed by this run.
 h.setJobs(JSON.parse(readFileSync(jobs,"utf8")));const before=h.claims().length;
 assert.equal(await h.poll(null,release),"");
 assert.equal(h.claims().length,before);assert.deepEqual(released,[["rel_lost","claimed",2,CODE_RUN]]);assert.equal(existsSync(join(h.home,"transport-wait.json")),false);
});

// Stale list snapshots on the poll's ledger read (wi_1ed244046bcbc1ce). The
// fake hub fails the list pages a rule names, counted per kind over the
// test: "first" is an active page with no token, "settled" the settled first
// page and "bookend" an active page with a token and no cursor. One verified
// job makes two bookends a pass: its detail's and the closing one.
const STALE_LIST="tt: hub: 409 conflict: release: stale list snapshot",CLI_FAILED="A deployer CLI call failed";
function staleHost(t,rules,extra=""){
 const cwd=mkdtempSync(join(tmpdir(),"release-stale-"));t.after(()=>rmSync(cwd,{recursive:true,force:true}));
 const faults=join(cwd,"faults.json");writeFileSync(faults,JSON.stringify({rules,seen:{}}));
 const h=codeHost(t,cwd,`const page=a[1]!=='list'?a[1]:flag('--view')==='settled'?(a.includes('--after')?'settled-next':'settled'):a.includes('--after')?'active-next':a.includes('--snapshot')?'bookend':'first';
const faults=JSON.parse(fs.readFileSync(${JSON.stringify(faults)},'utf8')),n=faults.seen[page]=(faults.seen[page]||0)+1;fs.writeFileSync(${JSON.stringify(faults)},JSON.stringify(faults));
const rule=faults.rules.find(r=>r.page===page&&n>=r.from&&(r.to===null||n<=r.to));if(rule){fs.writeSync(2,rule.stderr+'\\n');process.exit(1);}`+extra);
 const released=[],release=async c=>{released.push(c.job.id);return {jobId:c.job.id,outcome:"waiting_inputs"};};
 const pages=kind=>h.calls().filter(c=>c.argv[1]==="list"&&(kind==="first"?!c.argv.includes("--snapshot"):kind==="bookend"?c.argv.includes("active")&&c.argv.includes("--snapshot")&&!c.argv.includes("--after"):true)).length;
 const told=()=>h.sends().filter(s=>s.subject===CLI_FAILED),kept=()=>JSON.parse(readFileSync(join(h.home,"cli-failures.json"),"utf8")).failures;
 return {h,released,release,pages,told,kept};
}
test("stale s1 a bookend refused stale once restarts the whole read: the job runs, the poll is not held and nothing is noticed",async t=>{
 const s=staleHost(t,[{page:"bookend",from:1,to:1,stderr:STALE_LIST}]);s.h.setJobs([verifiedJob("rel_stale")]);
 assert.equal(await s.h.poll(null,s.release),"");assert.deepEqual(s.released,["rel_stale"]);
 // Two whole reads: the refused pass, then fresh summaries, the detail's bookend and the closing one.
 assert.equal(s.pages("first"),2);assert.equal(s.pages("bookend"),3);assert.equal(s.h.calls().filter(c=>c.argv[1]==="get").length,2);
 assert.deepEqual(s.told(),[]);
 // The private record still keeps the refused try.
 assert.deepEqual(s.kept().map(f=>[f.argv.slice(1,4).join(" "),f.stderr,f.count]),[["deployment list --view",STALE_LIST+"\n",1]]);
 // The closing bookend refused once restarts the read the same way.
 const c=staleHost(t,[{page:"bookend",from:2,to:2,stderr:STALE_LIST}]);c.h.setJobs([verifiedJob("rel_stale")]);
 assert.equal(await c.h.poll(null,c.release),"");assert.deepEqual(c.released,["rel_stale"]);assert.equal(c.pages("first"),2);assert.deepEqual(c.told(),[]);
});
test("stale s1 a bookend refused stale on every pass holds the poll after the bound with one notice, and nothing is claimed",async t=>{
 const s=staleHost(t,[{page:"bookend",from:1,to:null,stderr:STALE_LIST}]);s.h.setJobs([verifiedJob("rel_stale")]);
 assert.equal(await s.h.poll(null,s.release),"Deployment poll held (tt deployment list exit 1); inspect native input or recovery evidence.\n");
 assert.equal(s.pages("first"),RELEASE_READ_ATTEMPTS);assert.equal(s.pages("bookend"),RELEASE_READ_ATTEMPTS);assert.deepEqual(s.released,[]);assert.deepEqual(s.h.claims(),[]);
 assert.equal(s.told().length,1);assert.equal(s.told()[0].text,cliFailureNotice({reason:"tt deployment list exit 1"},CODE_RUN).text);
 assert.equal(s.kept().length,1);assert.equal(s.kept()[0].count,RELEASE_READ_ATTEMPTS);
});
test("stale s1 a bookend failure other than a stale snapshot holds the poll at once with no restart",async t=>{
 for(const stderr of ["tt: hub: 409 conflict: release: generation changed","tt: hub: 404 not found: release job"]){
  const s=staleHost(t,[{page:"bookend",from:1,to:null,stderr}]);s.h.setJobs([verifiedJob("rel_stale")]);
  assert.match(await s.h.poll(null,s.release),/^Deployment poll held \(tt deployment list exit 1\)/);
  assert.equal(s.pages("first"),1);assert.equal(s.pages("bookend"),1);assert.deepEqual(s.released,[]);assert.equal(s.told().length,1);
 }
 // A detail read that fails does not restart either.
 const g=staleHost(t,[{page:"get",from:1,to:null,stderr:"tt: hub: 404 not found: release job"}]);g.h.setJobs([verifiedJob("rel_stale")]);
 assert.match(await g.h.poll(null,g.release),/^Deployment poll held \(tt deployment get exit 1\)/);assert.equal(g.pages("first"),1);assert.equal(g.told().length,1);
});
test("stale s2 a stale refusal the read recovered from is not noticed when a later step of the same poll fails",async t=>{
 // The summary read recovers from a refused settled page, then the detail read fails: only the detail is noticed.
 const s=staleHost(t,[{page:"settled",from:1,to:1,stderr:STALE_LIST},{page:"get",from:1,to:null,stderr:"tt: hub: 404 not found: release job"}]);s.h.setJobs([verifiedJob("rel_stale")]);
 assert.match(await s.h.poll(null,s.release),/^Deployment poll held \(tt deployment get exit 1\)/);assert.equal(s.pages("first"),2);
 assert.deepEqual(s.told().map(n=>n.text),[cliFailureNotice({reason:"tt deployment get exit 1",jobId:null},CODE_RUN).text]);
 assert.deepEqual(s.kept().map(f=>f.argv[2]),["list","get"]);
 // A recovered bookend, then a release that is held: only the release's own failures would be noticed, and it has none.
 const r=staleHost(t,[{page:"bookend",from:1,to:1,stderr:STALE_LIST}]);r.h.setJobs([verifiedJob("rel_stale")]);
 assert.equal(await r.h.poll(null,async()=>{throw new Error("held");}),"Release held; inspect handler fence and private journal.\n");
 assert.deepEqual(r.told(),[]);assert.equal(r.kept().length,1);
});
test("stale s1 a later job's bookend refused stale in the job loop ends the poll quietly: no restart, claim, hold or notice",async t=>{
 // rel_first's claim is refused, so rel_next is read in the job loop; its bookend (the third) is refused stale.
 const s=staleHost(t,[{page:"bookend",from:3,to:3,stderr:STALE_LIST}],`if(a[1]==='claim'&&flag('--job')==='rel_first'){fs.writeSync(2,'tt: hub: 409 conflict: release: candidate changed\\n');process.exit(1);}`+heldRoster);
 s.h.setJobs([verifiedJob("rel_first"),verifiedJob("rel_next")]);
 assert.equal(await s.h.poll(null,s.release),"Release claim held; handler reconciliation required.\nRelease ledger changed during the poll; the next poll reads it again.\n");
 assert.equal(s.pages("first"),1);assert.deepEqual(s.h.claims().map(c=>c.argv[c.argv.indexOf("--job")+1]),["rel_first"]);assert.deepEqual(s.released,[]);assert.deepEqual(s.told(),[]);
 // The next poll reads afresh and runs the later job.
 assert.equal(await s.h.poll(null,s.release),"Release claim held; handler reconciliation required.\n");assert.deepEqual(s.released,["rel_next"]);assert.deepEqual(s.told(),[]);
});
