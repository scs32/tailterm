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
    def attach(command):
        master,slave=pty.openpty()
        env={**os.environ,'TERM':'xterm-256color','LC_ALL':'C','LANG':'C'}
        process=subprocess.Popen(['/bin/sh','-c',command],stdin=slave,stdout=slave,stderr=slave,env=env,start_new_session=True)
        os.close(slave)
        return process,master
    process,fd=attach(command)
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
        assert tmux('list-clients','-F','#{client_utf8}')=='1', 'Browser clients must use UTF-8 even when SSH has an ASCII locale'
        target_id, target_created = tmux('display-message','-p','-t','clipboard-test','#{session_id}|#{session_created}').split('|')
        history_script='import {tmuxHistoryCommand} from "./shared/tmux-command.js";process.stdout.write(tmuxHistoryCommand(JSON.parse(process.argv[2]),process.argv[1]));'
        history_command=subprocess.check_output(['node','--input-type=module','-e',history_script,wrapper,json.dumps({'id':target_id,'created':target_created})],text=True)
        before_history_mode=tmux('display-message','-p','-t','clipboard-test','#{pane_in_mode}')
        history=subprocess.run(['/bin/sh','-c',history_command],capture_output=True,text=True)
        assert history.returncode==0, history.stderr
        assert history.stdout.strip()==tmux('capture-pane','-p','-J','-S','-5000','-t','clipboard-test')
        assert tmux('display-message','-p','-t','clipboard-test','#{pane_in_mode}')==before_history_mode

        symbols='Unicode probe: ❯ ⌘ ⇧ ’ •'
        tmux('display-message','-d','5000',symbols)
        drain(.2)
        assert symbols.encode() in output, repr(output[-500:])
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
        process.wait(timeout=5);os.close(fd)
        resume_script='import {tmuxCommand} from "./shared/tmux-command.js";process.stdout.write(tmuxCommand("clipboard-test",process.argv[1],true));'
        resume_command=subprocess.check_output(['node','--input-type=module','-e',resume_script,wrapper],text=True)
        process,fd=attach(resume_command)
        output=b'';drain(.5)
        assert tmux('list-clients','-F','#{session_name}')=='clipboard-test'
        assert tmux('list-clients','-F','#{client_utf8}')=='1'
        def rename(old,new):
            script='import {tmuxRenameCommand} from "./shared/tmux-command.js";process.stdout.write(tmuxRenameCommand(process.argv[2],process.argv[3],process.argv[1]));'
            command=subprocess.check_output(['node','--input-type=module','-e',script,wrapper,old,new],text=True)
            return subprocess.run(['/bin/sh','-c',command],capture_output=True,text=True,timeout=5)
        identity=tmux('display-message','-p','-t','clipboard-test','#{session_id}|#{pane_id}|#{pane_pid}')
        session_id,created=tmux('display-message','-p','-t','clipboard-test','#{session_id}|#{session_created}').split('|')
        assert rename('clipboard-test','renamed-test').returncode==0
        assert tmux('display-message','-p','-t','renamed-test','#{session_id}|#{pane_id}|#{pane_pid}')==identity
        assert tmux('list-clients','-F','#{session_name}')=='renamed-test'
        assert process.poll() is None
        assert rename('renamed-test','fixture-keepalive').returncode!=0
        assert rename('renamed','wrong-prefix').returncode!=0
        assert tmux('display-message','-p','-t','renamed-test','#{session_id}|#{pane_id}|#{pane_pid}')==identity
        print('Real tmux rename: exact target, duplicate rejection, same session/pane/process, attached client preserved.')
        assert rename('renamed-test','-renamed-test').returncode==0
        assert rename('-renamed-test','restored-test').returncode==0
        tmux('detach-client','-s','restored-test');drain(.2);process.wait(timeout=5);os.close(fd)
        stable_script='import {tmuxCommand} from "./shared/tmux-command.js";process.stdout.write(tmuxCommand("clipboard-test",process.argv[1],true,{id:process.argv[2],created:process.argv[3]}));'
        stable_command=subprocess.check_output(['node','--input-type=module','-e',stable_script,wrapper,session_id,created],text=True)
        process,fd=attach(stable_command);drain(.5)
        assert tmux('list-clients','-F','#{session_name}')=='restored-test'
        stale_command=subprocess.check_output(['node','--input-type=module','-e',stable_script,wrapper,session_id,'0'],text=True)
        stale=subprocess.run(['/bin/sh','-c',stale_command],capture_output=True,text=True,timeout=5)
        assert stale.returncode==1 and 'original tmux session' in stale.stderr
        print('Stable tmux identity: resumed after rename; rejected a stale/reused session identity.')
        print('Real tmux: UTF-8 output under an ASCII SSH locale for launch and resume, clipboard policy, mouse selection, OSC52 Unicode payload, detach persistence passed.')
    finally:
        subprocess.run([wrapper,'kill-server'],capture_output=True)
        try: os.killpg(process.pid,signal.SIGKILL)
        except ProcessLookupError: pass
        os.close(fd)
        process.wait(timeout=5)
