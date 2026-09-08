#!/usr/bin/env python3
"""Deploy only the coordination app through TrueNAS middleware.
Uses an existing host address. Never installs or configures Tailscale.
Usage: scripts/deploy-truenas-hub.py RELEASE [--update]
Requires the dataset to exist and the compiled Linux amd64 hub in .build/ttbin.
"""
import json, pathlib, re, secrets, shlex, subprocess, sys
root = pathlib.Path(__file__).resolve().parents[1]
release = sys.argv[1]
if not re.fullmatch(r'[A-Za-z0-9._-]+', release):
    raise SystemExit('Invalid release name')
base = '/mnt/deepfreeze/tailterm-hub'
ssh = ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', 'truenas']
def remote(command, data=None):
    return subprocess.check_output(ssh + [command], input=data)
# The token is a host file, not an environment variable in the app definition.
remote(f'umask 077; test -s {base}/hub-token || cat > {base}/hub-token', (secrets.token_urlsafe(48)+'\n').encode())
remote(f'mkdir -p {base}/releases/{release}')
binary = root / '.build/ttbin/tailterm-hub-linux-amd64'
remote(f'cat > {base}/releases/{release}/tailterm-hub && chmod 755 {base}/releases/{release}/tailterm-hub', binary.read_bytes())
compose = {'services': {'hub': {
    'image': 'gcr.io/distroless/static-debian12:nonroot',
    'user': '950:950', 'restart': 'unless-stopped',
    'entrypoint': ['/opt/tailterm-hub'],
    'read_only': True,
    'cap_drop': ['ALL'], 'security_opt': ['no-new-privileges:true'],
    'ports': ['100.116.238.37:18765:18765'],
    'environment': {'TAILTERM_TCP_LISTEN': '0.0.0.0:18765', 'TAILTERM_STATE': '/state', 'TAILTERM_TOKEN_FILE': '/run/hub-token', 'TAILTERM_MAX_AGENTS': '32'},
    'volumes': [f'{base}/releases/{release}/tailterm-hub:/opt/tailterm-hub:ro', f'{base}/state:/state', f'{base}/hub-token:/run/hub-token:ro'],
    'mem_limit': '512m', 'cpus': '1.0',
}}}
request = {'custom_compose_config': compose}
if '--update' in sys.argv:
    command = 'midclt call -j app.update tailterm-hub '
else:
    request.update(app_name='tailterm-hub', custom_app=True)
    command = 'midclt call -j app.create '
remote(command + shlex.quote(json.dumps(request)))
remote("midclt call -j app.start tailterm-hub")
print('TrueNAS middleware deployed tailterm-hub at http://100.116.238.37:18765')
