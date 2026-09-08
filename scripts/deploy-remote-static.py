#!/usr/bin/env python3
"""Update the existing remote Mac preview using only changed public assets.

Usage: python3 scripts/deploy-remote-static.py theAir
The running tailterm-static container and its rollback image stay in place.
Assets are verified before index.html is switched. This avoids rebuilding or
copying the full image on a low-disk host or slow link. It changes no hub, vault,
agent sessions, Tailscale settings, or container configuration.
"""
import hashlib
import json
import pathlib
import re
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[1]
host = sys.argv[1] if len(sys.argv) == 2 else ""
if not re.fullmatch(r"[A-Za-z0-9._-]+", host):
    raise SystemExit("Supply an existing SSH host alias.")
subprocess.run(["npm", "run", "verify:release"], cwd=root, check=True)
site = root / "dist-static"
manifest_bytes = (site / "release.json").read_bytes()
manifest = json.loads(manifest_bytes)
commit = manifest["commit"]
if not re.fullmatch(r"[0-9a-f]{40}", commit):
    raise SystemExit("Expected an exact release commit.")
ssh = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o",
       "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", host]


def remote(script):
    result = subprocess.run(ssh + ["python3 -"], input=script, text=True,
                            capture_output=True, timeout=600)
    if result.returncode:
        sys.stderr.write(result.stderr)
        raise SystemExit(result.returncode)
    return json.loads(result.stdout)


previous = remote('''import json,subprocess,urllib.request
with urllib.request.urlopen('http://127.0.0.1:4318/release.json',timeout=15) as r:
    manifest=json.load(r)
info=json.loads(subprocess.check_output(['/opt/homebrew/bin/container','inspect','tailterm-static']))[0]
print(json.dumps({'manifest':manifest,'image':info['configuration']['image']['reference']}))
''')
changed = [name for name, item in manifest["files"].items()
           if previous["manifest"]["files"].get(name) != item] + ["release.json"]
for name in changed:
    if not re.fullmatch(r"[A-Za-z0-9_./-]+", name) or ".." in name.split("/"):
        raise SystemExit("Unsafe asset path.")
transfer_size = sum((site / name).stat().st_size for name in changed)
stage_relative = ".local/share/tailterm/web-releases/" + commit[:12]
remote(f'''import json,shutil
from pathlib import Path
stage=Path.home()/{stage_relative!r}
stage.mkdir(parents=True,exist_ok=True)
assert shutil.disk_usage(stage).free > {transfer_size * 3 + 20 * 1024 * 1024}, 'Not enough space for a verified delta update'
(stage/'site').mkdir(exist_ok=True)
print(json.dumps({{'ready':True}}))
''')
print(f"Transferring {len(changed)} changed assets ({transfer_size} bytes) to {host}.", flush=True)
archive = subprocess.Popen(["tar", "-czf", "-", "-C", str(site), *changed], stdout=subprocess.PIPE)
try:
    subprocess.run(ssh + [f"tar -xzf - -C ~/{stage_relative}/site"],
                   stdin=archive.stdout, check=True, timeout=600)
except BaseException:
    archive.terminate()
    archive.wait()
    raise
finally:
    archive.stdout.close()
if archive.wait() != 0:
    raise SystemExit("Asset archive transfer did not complete.")

parameters = {
    "commit": commit, "stage": stage_relative, "changed": changed,
    "previousCommit": previous["manifest"]["commit"],
    "releaseSha256": hashlib.sha256(manifest_bytes).hexdigest(),
    "image": previous["image"],
}
script = '''import hashlib,json,shlex,subprocess,urllib.request
from pathlib import Path
p=PARAMETERS
stage=Path.home()/p['stage']
site=stage/'site'
raw=(site/'release.json').read_bytes()
assert hashlib.sha256(raw).hexdigest()==p['releaseSha256']
manifest=json.loads(raw)
assert manifest['commit']==p['commit'] and not manifest['dirty']
def fetch(name):
    with urllib.request.urlopen('http://127.0.0.1:4318/'+name,timeout=20) as r:return r.read()
before=fetch('release.json')
assert json.loads(before)['commit']==p['previousCommit'], 'Preview changed while assets were transferring'
(stage/'previous-release.json').write_bytes(before)
(stage/'previous-index.html').write_bytes(fetch('index.html'))
for name in p['changed']:
    data=(site/name).read_bytes()
    expected={'sha256':p['releaseSha256'],'size':len(raw)} if name=='release.json' else manifest['files'][name]
    assert len(data)==expected['size'] and hashlib.sha256(data).hexdigest()==expected['sha256'],name
for name,expected in manifest['files'].items():
    if name in p['changed'] or name=='_headers':continue
    data=fetch(name)
    assert len(data)==expected['size'] and hashlib.sha256(data).hexdigest()==expected['sha256'],name
app='/opt/homebrew/bin/container'
destination='/tmp/tailterm-web-'+p['commit'][:12]
subprocess.run([app,'exec','tailterm-static','mkdir','-p',destination],check=True,stdout=subprocess.DEVNULL)
subprocess.run([app,'cp',str(site)+'/.','tailterm-static:'+destination],check=True,stdout=subprocess.DEVNULL)
# Apple Container preserves the source directory inside an existing destination.
copied=destination+'/site'
commands=[]
for name in p['changed']:
    expected=p['releaseSha256'] if name=='release.json' else manifest['files'][name]['sha256']
    path=copied+'/'+name
    commands.append('test "$(sha256sum '+shlex.quote(path)+' | cut -d " " -f 1)" = '+shlex.quote(expected))
# Verify the entire delta before changing any served file. Immutable assets go
# first, then the two small entry/receipt files are each renamed atomically.
for name in p['changed']:
    if name in ('index.html','release.json'):continue
    target='/srv/'+name
    commands.extend(['mkdir -p '+shlex.quote(str(Path(target).parent)),
                     'cp '+shlex.quote(copied+'/'+name)+' '+shlex.quote(target)])
for name in ('index.html','release.json'):
    if name in p['changed']:
        commands.extend(['cp '+shlex.quote(copied+'/'+name)+' '+shlex.quote('/srv/'+name+'.next'),
                         'mv '+shlex.quote('/srv/'+name+'.next')+' '+shlex.quote('/srv/'+name)])
subprocess.run([app,'exec','-i','tailterm-static','sh','-eu'],input='\\n'.join(commands)+'\\n',text=True,check=True,stdout=subprocess.DEVNULL)
assert hashlib.sha256(fetch('release.json')).hexdigest()==p['releaseSha256']
verified=0
for name,expected in manifest['files'].items():
    if name=='_headers':continue
    data=fetch(name)
    assert len(data)==expected['size'] and hashlib.sha256(data).hexdigest()==expected['sha256'],name
    verified+=1
subprocess.run([app,'exec','tailterm-static','rm','-rf',destination],check=True,stdout=subprocess.DEVNULL)
receipt={'host':HOST,'origin':'http://127.0.0.1:4318','commit':p['commit'],'releaseSha256':p['releaseSha256'],
         'verifiedAssets':verified,'mode':'changed static files in existing container','containerImage':p['image'],
         'rollbackImage':p['image'],'previousCommit':p['previousCommit'],'artifactDirectory':str(stage)}
(stage/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\\n')
print(json.dumps(receipt))
'''.replace("PARAMETERS", repr(parameters)).replace("HOST", repr(host))
print("Delta transferred; verifying and activating the static files.", flush=True)
receipt = remote(script)
(root / ".build" / f"remote-preview-{host}.json").write_text(json.dumps(receipt, indent=2) + "\n")
print(json.dumps(receipt))
