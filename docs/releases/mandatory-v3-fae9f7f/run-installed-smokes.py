from pathlib import Path
import os,subprocess,json,tempfile,hashlib
root=Path(__file__).resolve().parent
smoke=root/'installed-smokes'
assert hashlib.sha256((smoke/'tt-darwin-arm64').read_bytes()).hexdigest()=='7a63670a9210c7f5208cb500845e53d2f9fd42cc4f33d175c3221cf5d9d060c1'
cases=[('mandatory-v3-cli-smoke.py',['--output-dir',str(smoke/'mandatory-v3')]),('reader-cli-smoke.py',[str(smoke/'reader-result.json')]),('operational-cli-smoke.py',['--output-dir',str(smoke/'operational')]),('followthrough-cli-smoke.py',['--output-dir',str(smoke/'followthrough')])]
results=[]
with tempfile.TemporaryDirectory(prefix='mandatory-v3-installed-home-') as isolated:
 env={k:v for k,v in os.environ.items() if not k.startswith(('TAILTERM_','TT_','CODEX_')) and k not in ('TMUX','TMUX_PANE')}
 env.update(HOME=isolated,XDG_CONFIG_HOME=isolated+'/config',XDG_DATA_HOME=isolated+'/data',PYTHONDONTWRITEBYTECODE='1')
 for name,args in cases:
  with (smoke/(name+'.stdout')).open('x') as out,(smoke/(name+'.stderr')).open('x') as err:
   proc=subprocess.run(['python3',str(smoke/name),*args],env=env,stdout=out,stderr=err,timeout=90)
  results.append({'script':name,'exitCode':proc.returncode,'scriptSHA256':hashlib.sha256((smoke/name).read_bytes()).hexdigest()})
  print(name,proc.returncode,flush=True)
  if proc.returncode:break
(root/'installed-smoke-results.json').write_text(json.dumps(results,indent=2)+'\n')
assert len(results)==4 and all(x['exitCode']==0 for x in results)
