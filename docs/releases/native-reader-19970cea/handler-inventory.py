import json
import os
import pathlib
import subprocess
from datetime import datetime, timezone

if (os.environ.get('TAILTERM_AGENT'), os.environ.get('TAILTERM_RUN')) != ('agt_3760a4361c514a03', 'run_1a67a6c9be7c7136'):
    raise SystemExit('Execute only in the current db-handler session; do not override identity.')

source = '''import json,os,socket,sys
print(json.dumps({"targetHost":socket.gethostname(),"targetExecutable":os.path.realpath(sys.executable),"pythonVersion":sys.version.split()[0],"uid":os.getuid(),"gid":os.getgid()}))
'''
result = subprocess.run(['ssh','-o','BatchMode=yes','-o','ConnectTimeout=10','truenas','/usr/bin/python3','-'],input=source,text=True,capture_output=True,timeout=30)
record={'order':4831,'route':'truenas','observedAt':datetime.now(timezone.utc).isoformat(),'exitCode':result.returncode,'databaseAccess':False,'mutation':False}
if result.returncode:
    record['error']=result.stderr.strip()[-1000:]
else:
    record['inventory']=json.loads(result.stdout)
target=pathlib.Path(__file__).with_name('handler-host-inventory.json')
with target.open('x') as handle:
    os.chmod(target,0o600)
    json.dump(record,handle,indent=2)
    handle.write('\n')
print(json.dumps(record))
raise SystemExit(0 if result.returncode == 0 else 1)
