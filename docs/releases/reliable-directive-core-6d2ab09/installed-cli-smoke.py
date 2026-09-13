import http.server, threading, subprocess, os, pathlib, json, hashlib
cli=pathlib.Path.home()/'.local/bin/tt'
assert hashlib.sha256(cli.read_bytes()).hexdigest()=='f67b7dc13c99feeacd1801c394f67d7a137529d3d5a990bfeacf4ca6ec76a6c2'
task='tsk_0000000000000001';agent='agt_0000000000000002';run='run_0000000000000003';delivery='dly_0000000000000004'
mode='supported'; requests=[]
class Handler(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args): pass
 def send(self,obj,status=200):
  b=json.dumps(obj).encode();self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(b)));self.end_headers();self.wfile.write(b)
 def do_GET(self):
  assert self.headers.get('Authorization')=='Bearer synthetic-cli-smoke'
  requests.append({'method':'GET','path':self.path})
  if self.path=='/v1/capabilities':
   self.send({'schemaVersion':1,'reliableDelivery':{'supported':mode!='unsupported','versions':[1] if mode=='supported' else [999]}});return
  assert self.path==f'/v1/tasks/{task}/agents/{agent}/current-assignment?runId={run}'
  self.send({'id':delivery,'generation':2,'kind':'assignment','phase':'acknowledged','executionEpoch':2,'messageSeq':1,'itemId':'wi_0000000000000005','itemRevision':1,'workOrderMessage':{'taskId':task,'seq':1},'message':{'text':'synthetic stored task data'}})
 def do_POST(self):
  assert self.headers.get('Authorization')=='Bearer synthetic-cli-smoke'
  obj=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
  assert self.path==f'/v1/tasks/{task}/deliveries/{delivery}/progress'
  assert obj=={'requestId':'installed-smoke-progress','agentId':agent,'runId':run,'expectedEpoch':2,'text':'synthetic progress'}
  requests.append({'method':'POST','path':self.path,'requestId':obj['requestId'],'expectedEpoch':obj['expectedEpoch']})
  self.send({'delivery':{'id':delivery,'generation':2,'phase':'progressing','executionEpoch':2},'event':{'kind':'progress'},'receipt':{'id':'drcp_0000000000000006'},'replay':False})
s=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);threading.Thread(target=s.serve_forever,daemon=True).start()
e={k:v for k,v in os.environ.items() if not k.startswith('TAILTERM_')};e.update(TAILTERM_HUB=f'http://127.0.0.1:{s.server_port}',TAILTERM_TOKEN='synthetic-cli-smoke',TAILTERM_TASK=task,TAILTERM_AGENT=agent,TAILTERM_RUN=run)
results=[]
def call(args):return subprocess.run([str(cli),*args],env=e,text=True,capture_output=True,timeout=15)
r=call(['current-assignment']);assert r.returncode==0 and 'execution epoch 2' in r.stdout and 'synthetic stored task data' in r.stdout;results.append('installed current-assignment displays fixture epoch2')
r=call(['delivery','progress','--request-id','installed-smoke-progress','--expected-epoch','2','--text','synthetic progress',delivery]);assert r.returncode==0 and 'phase=progressing' in r.stdout;results.append('documented flags-before-ID sends exact epoch2/body to isolated fixture')
for value in ['unsupported','unknown']:
 mode=value; before=len(requests);r=call(['delivery','progress','--request-id','must-not-send','--expected-epoch','2','--text','blocked',delivery]);assert r.returncode!=0 and 'upgrade' in r.stderr.lower();assert len(requests)==before+1 and requests[-1]['path']=='/v1/capabilities';results.append(value+' capability fails closed before action')
s.shutdown();s.server_close()
out={'installedSha256':hashlib.sha256(cli.read_bytes()).hexdigest(),'isolatedHTTPOnly':True,'liveCredentialsUsed':False,'liveHubFixtureCreated':False,'results':results,'requests':requests,'limit':'This verifies the actual installed CLI serialization/output/capability gate, not a second store semantic acceptance test; QA3938 covers actual CLI-HTTP-store.'}
pathlib.Path(__file__).with_suffix('.json').write_text(json.dumps(out,indent=2)+'\n');print(json.dumps({'pass':len(results),'scope':out['limit']}))
