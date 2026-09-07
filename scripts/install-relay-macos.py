#!/usr/bin/env python3
"""Install the per-user Tailterm inbox relay on a Mac with ~/.local/bin/tt.
Run locally, or pipe this script to `ssh HOST python3 -`. No system services,
Codex configuration, or Tailscale settings are modified.
"""
import os, pathlib, plistlib, subprocess
home=pathlib.Path.home()
binary=home/'.local/bin/tt'
if not binary.is_file(): raise SystemExit('Install ~/.local/bin/tt first')
label='com.tailterm.inbox-relay'
domain='gui/'+str(os.getuid())
subprocess.run(['launchctl','print',domain],check=True,stdout=subprocess.DEVNULL)
logs=home/'Library/Logs/Tailterm';logs.mkdir(parents=True,exist_ok=True)
folder=home/'Library/LaunchAgents';folder.mkdir(parents=True,exist_ok=True)
path=folder/(label+'.plist')
config={'Label':label,'ProgramArguments':[str(binary),'relay'],'WorkingDirectory':str(home),
 'EnvironmentVariables':{'PATH':str(home/'.local/bin')+':/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin'},
 'RunAtLoad':True,'KeepAlive':True,'ThrottleInterval':15,
 'StandardOutPath':str(logs/'inbox-relay.log'),'StandardErrorPath':str(logs/'inbox-relay.log')}
staged=path.with_suffix('.new');staged.write_bytes(plistlib.dumps(config));staged.chmod(0o600);staged.replace(path)
subprocess.run(['launchctl','bootout',domain+'/'+label],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
subprocess.run(['launchctl','bootstrap',domain,str(path)],check=True)
print('Installed and started '+label+' for this user.')
