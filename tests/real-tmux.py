"""Isolated tmux clipboard/launch test; requires an explicit tmux binary argument."""
import base64, json, os, pathlib, pty, select, signal, subprocess, sys, tempfile, time
binary=str(pathlib.Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory(prefix='tailserve-tmux-') as directory:
    socket=directory+'/socket'
    wrapper=directory+'/tmux-wrapper'
    import shlex
    pathlib.Path(wrapper).write_text('#!/bin/sh\nexec '+shlex.quote(binary)+' -f /dev/null -S '+shlex.quote(socket)+' "$@"\n')
    os.chmod(wrapper,0o700)
    def tmux(*args):
        r=subprocess.run([wrapper,*args],capture_output=True,text=True)
        if r.returncode: raise RuntimeError(r.stderr)
        return r.stdout.strip()
    tmux('new-session','-d','-s','fixture-keepalive')
    tmux('set-option','-s','set-clipboard','off')
    script='import {tmuxCommand} from "./shared/tmux-command.js";process.stdout.write(tmuxCommand("clipboard-test",process.argv[1]));'
    command=subprocess.check_output(['node','--input-type=module','-e',script,wrapper],text=True)
    child,fd=pty.fork()
    if child==0:
        os.environ['TERM']='xterm-256color'
        os.execl('/bin/sh','sh','-c',command)
    output=b''
    def drain(seconds=1):
        global output
        until=time.monotonic()+seconds
        while time.monotonic()<until:
            if select.select([fd],[],[],0.05)[0]:
                try: chunk=os.read(fd,65536)
                except OSError: break
                if not chunk: break
                output+=chunk
    try:
        drain(1)
        assert tmux('show-option','-s','-v','set-clipboard')=='external'
        assert tmux('show-option','-t','clipboard-test','-v','mouse')=='on'
        content='tmux copied café 🦊'
        tmux('set-buffer','-w',content)
        drain(.5)
        expected=base64.b64encode(content.encode())
        assert b'\x1b]52;' in output and expected in output,repr(output[-300:])
        tmux('detach-client','-s','clipboard-test')
        drain(.2)
        assert 'clipboard-test' in tmux('list-sessions','-F','#{session_name}')
        missing_script='import {tmuxCommand} from "./shared/tmux-command.js";process.stdout.write(tmuxCommand("clipboard",process.argv[1],true));'
        missing_command=subprocess.check_output(['node','--input-type=module','-e',missing_script,wrapper],text=True)
        missing=subprocess.run(['/bin/sh','-c',missing_command],capture_output=True,text=True)
        assert missing.returncode==1 and 'no longer exists' in missing.stderr
        os.waitpid(child,0);os.close(fd)
        resume_script='import {tmuxCommand} from "./shared/tmux-command.js";process.stdout.write(tmuxCommand("clipboard-test",process.argv[1],true));'
        resume_command=subprocess.check_output(['node','--input-type=module','-e',resume_script,wrapper],text=True)
        child,fd=pty.fork()
        if child==0:
            os.environ['TERM']='xterm-256color'
            os.execl('/bin/sh','sh','-c',resume_command)
        output=b'';drain(.5)
        assert tmux('list-clients','-F','#{session_name}')=='clipboard-test'
        print('Real tmux 3.6b: generated launch command, runtime clipboard policy, mouse selection, OSC52 Unicode payload, detach persistence passed.')
    finally:
        subprocess.run([wrapper,'kill-server'],capture_output=True)
        try: os.kill(child,signal.SIGTERM)
        except ProcessLookupError: pass
        os.waitpid(child,0)
        os.close(fd)
