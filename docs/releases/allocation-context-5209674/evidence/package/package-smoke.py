import copy,hashlib,json,os,sys,tempfile,threading,subprocess,socket
from pathlib import Path
from http.server import HTTPServer,BaseHTTPRequestHandler
binary=Path(sys.argv[1]).resolve(); output=Path(sys.argv[2]).resolve()
requests=[]; drop=[False]
class Server(BaseHTTPRequestHandler):
 def log_message(self,*args): pass
 def do_POST(self):
  body=self.rfile.read(int(self.headers.get('Content-Length','0')))
  requests.append({'path':self.path,'body':json.loads(body)})
  if drop[0]:
   drop[0]=False;self.connection.shutdown(socket.SHUT_RDWR);self.connection.close();return
  self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(b'{}')
server=HTTPServer(('127.0.0.1',0),Server);threading.Thread(target=server.serve_forever,daemon=True).start()
task='tsk_1111111111111111';item='wi_1111111111111111'
rev={'itemId':item,'taskId':task,'kind':'bug','title':'synthetic release6142','status':'open','priority':'normal','itemSeq':1,'revision':1,'attributionKind':'shared_workspace_claim','changeKind':'created','provenance':'native'}
context={'version':1,'itemTaskId':task,'itemId':item,'itemRevision':1,'workOrderMessage':{'taskId':task,'seq':7},'history':{'revision':rev,'revisions':[rev],'gaps':[],'messages':[{'itemRevision':1,'revisionCoverage':'verified','relationship':'primary','message':{'taskId':task,'seq':7}}],'coverage':{'complete':True,'observedCurrentRevision':1,'latestMaterialized':1,'snapshotCount':1,'conversationLinks':'explicit_only'}}}
results=[]
with tempfile.TemporaryDirectory(prefix='allocation6142-synthetic-') as private:
 env={'PATH':'/usr/bin:/bin','HOME':private,'XDG_CONFIG_HOME':private+'/config','TAILTERM_HUB':f'http://127.0.0.1:{server.server_port}','TAILTERM_TASK':task,'TAILTERM_AGENT':'agt_1111111111111111','TAILTERM_RUN':'run_1111111111111111'}
 args=[str(binary),'allocation-intent','create','--agent-id','agt_2222222222222222','--work-item',item,'--work-item-revision','1','--work-order-message','7','--team-role','member','--expected-run-id','run_2222222222222222','--launcher-agent-id','agt_3333333333333333','--launcher-run-id','run_3333333333333333','--request-id','synthetic6142-fixed','--json']
 def run(name,raw,transport,valid=True,lost=False):
  path=Path(private)/'context.json';path.write_text(raw)
  start=len(requests);drop[0]=lost
  command=args+(['--work-context-file',str(path)] if transport=='file' else ['--work-context-json',raw])
  p=subprocess.run(command,env=env,cwd=private,capture_output=True,text=True,timeout=15)
  calls=requests[start:]
  if valid:
   assert len(calls)==1,(name,calls,p.stderr)
   body=calls[0]['body']; canonical=json.dumps(json.loads(raw),separators=(',',':'),ensure_ascii=False).encode()
   assert body['contextDigest']==hashlib.sha256(canonical).hexdigest(),body
   for k,v in {'requestId':'synthetic6142-fixed','expectedRunId':'run_2222222222222222','agentId':'agt_2222222222222222','expectedLauncherAgentId':'agt_3333333333333333','expectedLauncherRunId':'run_3333333333333333'}.items():assert body[k]==v,(k,body)
   assert (p.returncode!=0 if lost else p.returncode==0),(name,p.stderr)
  else:assert p.returncode!=0 and not calls,(name,p.returncode,calls)
  results.append({'name':name,'transport':transport,'exitCode':p.returncode,'allocationRequests':len(calls),'passed':True})
  return calls
 raw=json.dumps(context,indent=2)
 file=run('valid',raw,'file'); inline=run('valid',raw,'inline');assert file==inline
 lost=run('lost-response',raw,'file',lost=True);retry=run('retry',raw,'file');assert lost==retry
 for name,data in [('board-only',{'messages':[]}),('null',None),('mismatch',{**context,'itemRevision':2}),('missing-history',{k:v for k,v in context.items() if k!='history'})]:
  for transport in ['file','inline']:run(name,json.dumps(data),transport,False)
 for transport in ['file','inline']:run('malformed','{oops',transport,False)
server.shutdown();server.server_close()
report={'workItem':'wi_5209b017e66bbf20','releaseOrder':6142,'binaryPath':str(binary),'binarySha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'syntheticOnly':True,'tests':results,'passed':True,'limits':'Loopback transport test verifies request equality after a dropped response; store replay semantics remain covered by independent source QA.'}
output.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report,indent=2))
