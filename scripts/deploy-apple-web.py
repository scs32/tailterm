#!/usr/bin/env python3
"""Build/test the compiled webpage, then replace only the local web container.
The hub and its storage are untouched. Keeps the previous image for rollback.
"""
import json, pathlib, shutil, subprocess, time, urllib.request
root = pathlib.Path(__file__).resolve().parents[1]
def run(*args, **kw):
    return subprocess.check_output(args, cwd=root, **kw)
run('npm', 'run', 'verify:release')
commit = json.loads((root/'dist-static/release.json').read_text())['commit']
image = 'tailterm-static:' + commit[:12]
stage = root / '.build' / ('web-' + commit[:12])
stage.mkdir(parents=True, exist_ok=True)
shutil.copytree(root/'dist-static', stage/'site', dirs_exist_ok=True)
shutil.copy2(root/'deploy/Caddyfile.container', stage/'Caddyfile')
(stage/'Dockerfile').write_text('FROM caddy:2-alpine\nCOPY site /srv\nCOPY Caddyfile /etc/caddy/Caddyfile\n')
run('container', 'build', '-t', image, str(stage))
previous = json.loads(run('container', 'inspect', 'tailterm-static'))[0]['configuration']['image']['reference']
(stage/'rollback.json').write_text(json.dumps({'image':previous,'port':4318}))
run('container', 'run', '-d', '--name', 'tailterm-static-candidate', '-p', '127.0.0.1:4319:80', image)
def verify(port):
    for i in range(30):
        try:
            with urllib.request.urlopen(f'http://127.0.0.1:{port}/release.json',timeout=2) as f:
                assert json.load(f)['commit'] == commit
            return
        except (OSError, AssertionError):
            if i == 29: raise
            time.sleep(.5)
try:
    verify(4319)
    print('Candidate webpage verified.', flush=True)
    run('container', 'stop', 'tailterm-static', timeout=30)
    run('container', 'rm', 'tailterm-static')
    try:
        run('container', 'run', '-d', '--name', 'tailterm-static', '-p', '127.0.0.1:4318:80', image)
        verify(4318)
    except Exception:
        # Keep the known-good image available even if candidate activation fails.
        print('Activation failed. Previous image: ' + previous, flush=True)
        raise
finally:
    run('container', 'stop', 'tailterm-static-candidate', timeout=30)
    run('container', 'rm', 'tailterm-static-candidate')
print('Webpage deployed: http://127.0.0.1:4318/ (' + commit[:12] + ')')
print('Rollback image: ' + previous)
