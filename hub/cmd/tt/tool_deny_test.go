package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Every command in this file is a string handed to the classifier or to the
// hook as tool input. None is run, so no process is ever signalled. The one
// command the file does run is the documented jq summary, on temp files.

func bashPre(id, command string) map[string]any {
	return map[string]any{"session_id": "s1", "tool_use_id": id, "tool_name": "Bash", "tool_input": map[string]any{"command": command}}
}

func wouldDenyRows(rows []map[string]any) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		if row["outcome"] == "would_deny" {
			out = append(out, row)
		}
	}
	return out
}

func denyRuleNames(matches []toolDenyMatch) string {
	var names []string
	for _, m := range matches {
		names = append(names, m.rule)
	}
	return strings.Join(names, ",")
}

const toolDenyHeredocNearMiss = "cat > notes.txt <<'EOF'\npkill node\nnpm run deploy:static\nssh truenas tailscale up\nEOF\necho done"

// a1, a5: every rule fires on its fixtures and on none of its near-misses.
func TestToolDenyClassify(t *testing.T) {
	fires := []struct{ rules, command string }{
		{"host-pkill", `pkill node`},
		{"host-pkill", `pkill -9 -f "go test"`},
		{"host-pkill", `killall -TERM tt`},
		{"host-pkill", `sudo /usr/bin/pkill tmux`},
		{"host-pkill", `cd x && pkill -f vite`},
		{"host-pkill", `bash -c 'killall node'`},
		{"host-pkill", `FOO=1 timeout 5 pkill node 2>/dev/null || true`},
		{"host-pkill", `if pkill node; then echo gone; fi`},
		{"host-pkill", "echo start\npkill node"},
		{"host-pkill", `echo $(pkill node)`},
		{"host-pkill", `pgrep node | xargs -n 1 pkill`},
		{"host-pkill", `eval "pkill node"`},
		{"host-pkill", `ssh air 'pkill node'`},

		{"deployer-signal", `tmux kill-session -t tt-deployer-compact`},
		{"deployer-signal", `tmux send-keys -t tt-deployer-11ee78b6de77e2da C-c`},
		{"deployer-signal", `kill $(tmux list-panes -t tt-deployer-compact -F '#{pane_pid}')`},
		{"deployer-signal", "kill -9 `tmux list-panes -t tt-deployer-compact -F '#{pane_pid}'`"},
		{"deployer-signal", `tmux -L main killp -t tt-deployer-compact:0.0`},
		{"host-pkill,deployer-signal", `pkill -f tt-deployer`},

		{"old-site-deploy", `npm run deploy:static`},
		{"old-site-deploy", `npm --prefix . run-script deploy:static`},
		{"old-site-deploy", `node scripts/deploy-static.mjs`},
		{"old-site-deploy", `cd repo && node ./scripts/deploy-static.mjs --dry`},
		{"old-site-deploy", `pnpm run deploy:static`},
		{"old-site-deploy", `node scripts/deploy-static.mjs --check`},
		{"old-site-deploy", `./scripts/deploy-static.mjs`},

		{"truenas-tailscale", `ssh truenas tailscale status`},
		{"truenas-tailscale", `ssh truenas_admin@truenas-tailarr 'sudo tailscale up --ssh'`},
		{"truenas-tailscale", `ssh -o BatchMode=yes truenas "midclt call app.query; systemctl restart tailscaled"`},
		{"truenas-tailscale", `ssh truenas_admin@100.116.238.37 tailscale status`},
		{"truenas-tailscale", `ssh -p 22 TrueNAS.local Tailscale down`},

		{"host-pkill,deployer-signal,old-site-deploy,truenas-tailscale", `pkill -f tt-deployer; npm run deploy:static && ssh truenas tailscale status`},

		// k2: a heredoc fed to a shell, here or over ssh, is its script.
		{"host-pkill", "bash <<'EOF'\npkill node\nEOF"},
		{"host-pkill", "sudo bash -s -- one <<EOF\necho start\nkillall node\nEOF\necho done"},
		{"host-pkill", "ssh air <<'EOF'\npkill node\nEOF"},
		{"truenas-tailscale", "ssh truenas <<'EOF'\nsudo tailscale up\nEOF"},
		{"truenas-tailscale", "ssh truenas 'bash -s' <<-EOF\n\ttailscale up\n\tEOF"},
		{"host-pkill,truenas-tailscale", "ssh truenas sudo sh <<'EOF'\ntailscale down; pkill node\nEOF"},
		// k2: nice, yarn without run, a second tmux command, and a shift that is no heredoc.
		{"host-pkill", `nice pkill node`},
		{"host-pkill", `nice -n 10 sudo pkill node`},
		{"old-site-deploy", `yarn deploy:static`},
		{"old-site-deploy", `yarn --cwd site deploy:static`},
		{"deployer-signal", `tmux new -d \; kill-session -t tt-deployer-compact`},
		{"deployer-signal", `tmux -L main new -d \; send-keys -t x y \; killw -t tt-deployer-compact:1`},
		{"host-pkill", "(( n << 2 ))\npkill node"},
		{"host-pkill", "x=$(( n << 2 ))\npkill node"},
		{"host-pkill", "for (( i = 0; i < 1 << n; i++ )); do\npkill node\ndone"},
		// What k1 leaves alone still runs: a command after or inside each form.
		{"host-pkill", `files=(a b); pkill node`},
		{"host-pkill", `files=($(pkill node) b)`},
		{"host-pkill", `case "$x" in a|b) pkill node ;; esac`},
		{"host-pkill", `case "$x" in a) echo a ;; *) echo b; esac; pkill node`},
		{"host-pkill", `case "$x" in $(pkill node)) echo a ;; esac`},
		{"host-pkill", `if [[ $x =~ ^(a|b)$ ]]; then pkill node; fi`},
		{"host-pkill", `[[ -f x ]]&& pkill node`},
		{"host-pkill", `[[ -f x || -f y ]] || pkill node`},
		{"host-pkill", `((pkill node); echo done)`},
		{"host-pkill", `echo $(( $(pkill node; echo 1) + 1 ))`},
		{"deployer-signal", `tmux send-keys -t tt-deployer-compact 'check status' Enter`},
		{"deployer-signal", `tmux kill-session -ttt-deployer-compact`},
		{"deployer-signal", `pkill -f tt-deployer -u "$USER"`},
	}
	for _, c := range fires {
		t.Run("fires/"+c.command, func(t *testing.T) {
			if got := denyRuleNames(toolDenyClassify(c.command)); got != c.rules {
				t.Fatalf("rules = %q; want %q", got, c.rules)
			}
		})
	}
	nearMisses := []string{
		// host-pkill
		`pkill -U self node`,
		`pkill -u "$USER" -f vite`,
		`pkill -P 12345 node`,
		`pkill -TERM -P $$ node`,
		`kill 12345`,
		`kill -TERM "$pid"`,
		`pgrep -f node`,
		`echo pkill node`,
		`git commit -m "never run pkill node"`,
		`grep -rn killall docs`,
		`killall -l`,
		`killall -s node`,
		`killall -u stephen node`,
		`command -v pkill`,
		`echo ok # pkill node`,
		toolDenyHeredocNearMiss,
		"git commit -m \"$(cat <<'EOF'\nDon't run pkill node (or killall node)\nEOF\n)\"",
		// k1: words that are values, patterns or operands, and calls that signal nothing.
		`files=(scripts/deploy-static.mjs x)`,
		`cmds+=(pkill killall)`,
		"declare -a cmds=(\n  pkill\n  'killall node'\n)",
		`case "$x" in pkill|killall) echo no ;; esac`,
		`case $x in a) echo a ;; pkill) echo b ;; (killall | pkill) echo c ;& *) echo d ;; esac`,
		"case $x in\n  pkill)\n    echo b\n    ;;\n  killall) echo c\nesac",
		`echo $(case $x in pkill) echo b ;; esac)`,
		`[[ $x =~ ^(pkill|killall)$ ]]`,
		`if [[ $x == pkill || $x =~ (killall|pkill) ]]; then echo yes; fi`,
		`[[ $x < pkill ]] && echo yes`,
		`pkill --help`,
		`pkill -0 -f vite`,
		`killall -d node`,
		`killall --version`,
		`pkill -0 -f tt-deployer`,
		`kill -0 $(tmux list-panes -t tt-deployer-compact -F '#{pane_pid}')`,
		// A heredoc that is data, not the script.
		"bash run.sh <<'EOF'\npkill node\nEOF",
		"bash -c 'cat' <<'EOF'\npkill node\nEOF",
		"ssh truenas cat <<'EOF'\ntailscale up\nEOF",
		"ssh air 'cat > notes.txt' <<'EOF'\npkill node\nEOF",
		"bash 3<<'EOF'\npkill node\nEOF",
		`echo $(( 1 << 2 )) pkill node`,
		// deployer-signal
		`tmux has-session -t tt-deployer-compact`,
		`tmux capture-pane -p -t tt-deployer-compact`,
		`tmux ls | grep tt-deployer`,
		`tmux kill-session -t tt-builder-1`,
		`tmux -L test kill-server`,
		`echo tt-deployer-compact; kill 12345`,
		`tmux send-keys -t tt-builder-1 'check tt-deployer-compact status' Enter`,
		`tmux send-keys -t tt-builder-1 -- -t tt-deployer-compact`,
		`tmux new -s tt-deployer-compact \; kill-session -t tt-builder-1`,
		`tmux kill-session -t tt-builder-1 \; has-session -t tt-deployer-compact`,
		// old-site-deploy
		`npm run deploy:tailos`,
		`npm run build`,
		`cat scripts/deploy-static.mjs`,
		`grep -n deploy:static package.json`,
		`node --check scripts/deploy-static.mjs`,
		`node -e "console.log(1)" scripts/deploy-static.mjs`,
		`git log -- scripts/deploy-static.mjs`,
		`tt send --subject "do not run npm run deploy:static"`,
		`npm deploy:static`,
		`pnpm deploy:static`,
		`yarn add deploy:static`,
		// truenas-tailscale
		`tailscale status`,
		`tailscale ip -4`,
		`ssh truenas midclt call app.config tailterm-hub`,
		`ssh air tailscale status`,
		`ssh 100.64.0.9 tailscale status`,
		`ssh 100.116.238.37 uptime`,
		`echo "ssh truenas tailscale up"`,
		`ssh -i truenas.key air tailscale status`,
	}
	for _, command := range nearMisses {
		t.Run("near-miss/"+command, func(t *testing.T) {
			if got := toolDenyClassify(command); len(got) != 0 {
				t.Fatalf("rules = %q; want none", denyRuleNames(got))
			}
		})
	}
}

// m2: a help, version or signal-zero word silences kill, pkill and killall
// only where the tool reads it: as the first argument, and for killall's
// own options anywhere before its first operand.
func TestToolDenySilencingWords(t *testing.T) {
	for _, c := range []struct{ rules, command string }{
		{"host-pkill", `pkill -f '--help'`},
		{"host-pkill", `pkill -9 -f node -0`},
		{"deployer-signal", `killall -s KILL tt-deployer-compact`},
		{"host-pkill", `pkill -f -- --help`},
		{"host-pkill", `pkill node --version`},
		{"host-pkill", `pkill -f -0 vite`},
		{"host-pkill", `killall node -d`},
		{"host-pkill", `killall node -0`},
		{"host-pkill,deployer-signal", `killall tt-deployer-compact --help`},
		{"deployer-signal", `killall -9 tt-deployer-compact -l`},
		{"deployer-signal", `kill -9 tt-deployer -0`},
		{"deployer-signal", `kill -TERM tt-deployer --help`},
		{"deployer-signal", `killall -s 9 tt-deployer`},
		{"deployer-signal", `ssh air 'killall -s TERM tt-deployer-compact'`},
	} {
		if got := denyRuleNames(toolDenyClassify(c.command)); got != c.rules {
			t.Errorf("%s: rules = %q; want %q", c.command, got, c.rules)
		}
	}
	for _, command := range []string{
		`pkill --help`, `pkill --version`, `pkill -0 -f vite`, `pkill -0 -f tt-deployer`, `sudo pkill --help`,
		`kill --help`, `kill --version tt-deployer`, `kill -0 tt-deployer`,
		`killall --help`, `killall --version`, `killall -0 node`, `killall -v -0 node`, `killall -0 tt-deployer-compact`,
		`killall -d node`, `killall -v -d tt-deployer`, `killall -KILL -d node`, `killall -l`, `killall -q -l`,
		`killall -s node`, `killall -s tt-deployer-compact`, `killall -v -s tt-deployer-compact`, `killall -s 0 tt-deployer`,
		`killall -u stephen -d tt-deployer`, `killall -c tt -s tt-deployer`,
	} {
		if got := toolDenyClassify(command); len(got) != 0 {
			t.Errorf("%s: rules = %q; want none", command, denyRuleNames(got))
		}
	}
}

// A command the parser cannot follow matches nothing, and no input makes
// it panic or run long.
func TestToolDenyClassifyBounds(t *testing.T) {
	deep := strings.Repeat("$(", toolDenyMaxNest+1) + "pkill node" + strings.Repeat(")", toolDenyMaxNest+1)
	if got := toolDenyClassify(deep); len(got) != 0 {
		t.Fatalf("a command nested past the bound matched %q", denyRuleNames(got))
	}
	if got := denyRuleNames(toolDenyClassify(`echo $(echo $(pkill node))`)); got != "host-pkill" {
		t.Fatalf("nested substitution = %q", got)
	}
	// Each level holds the next both as a substitution and as a shell argument.
	inner := "pkill node"
	for i := 0; i < toolDenyMaxNest-1; i++ {
		inner = `eval "$(` + inner + `)"`
	}
	start := time.Now()
	toolDenyClassify(strings.Repeat(inner+"; ", toolDenyMaxCommand/len(inner+"; ")))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a deeply nested command took %v to classify", elapsed)
	}
	for _, command := range []string{"", `"`, `'`, "`", `$(`, `${`, `$'`, `\`, `<<`, `<<-`, "a <<EOF", `>`, `&>`, `2>&1`, `(`, `)`, `{`, `}`, `ssh`, `ssh -o`, `tmux`, `npm`, `node`, `bash -c`, `timeout`, `sudo -u`, `pkill --`, "ssh truenas \"tailscale\x00", strings.Repeat("(", 5000), strings.Repeat(`"$(`, 5000)} {
		toolDenyClassify(command)
		for i := range command {
			toolDenyClassify(command[i:] + " pkill node; " + command[:i])
		}
	}
}

// a2: a would-deny call leaves one row per matched rule with the rule, the
// kind, the agent, the time and a summary, beside its ordinary row.
func TestToolDenyRow(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)   // the row's fields are the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	before := time.Now().UTC().Add(-time.Second)
	toolHook(t, e, "PreToolUse", bashPre("u1", "pkill node"))
	rows := toolLedgerRows(t, root, e.agent)
	if len(rows) != 1 {
		t.Fatalf("rows after a matching pre = %v; want one", rows)
	}
	row := rows[0]
	want := map[string]any{
		"v": float64(1), "task": e.task, "agent": e.agent, "run": e.runID, "session": "s1", "tool": "Bash", "toolUseId": "u1",
		"argsDigest": toolArgsDigest(json.RawMessage(`{"command":"pkill node"}`)),
		"outcome":    "would_deny", "denyRule": "host-pkill", "denyKind": "pkill", "denySummary": "pkill (+1 args)",
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %v; want %v", k, row[k], v)
		}
	}
	if len(row) != len(want)+1 {
		t.Errorf("row fields = %s; want those of %v and time", rowKeys(row), want)
	}
	at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(row["time"]))
	if err != nil || at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("time = %v (%v); want now", row["time"], err)
	}
	if pending := toolPendingFiles(t, root, e.agent); len(pending) != 1 {
		t.Fatalf("pending files = %d; want the call's one", len(pending))
	}
	// The call then finishes as any other call does.
	post := bashPre("u1", "pkill node")
	toolHook(t, e, "PostToolUse", post)
	rows = toolLedgerRows(t, root, e.agent)
	if len(rows) != 2 || rows[1]["outcome"] != "ok" || rows[1]["denyRule"] != nil || rows[1]["denySummary"] != nil || rows[1]["durationSource"] != "measured" {
		t.Fatalf("rows after the post = %v", rows)
	}
	if pending := toolPendingFiles(t, root, e.agent); len(pending) != 0 {
		t.Fatalf("pending files after the post = %d", len(pending))
	}

	// One row per rule, in rule order.
	toolHook(t, e, "PreToolUse", bashPre("u2", "pkill -f tt-deployer; npm run deploy:static; ssh truenas tailscale status"))
	var got []string
	for _, row := range wouldDenyRows(toolLedgerRows(t, root, e.agent))[1:] {
		if row["toolUseId"] != "u2" || row["agent"] != e.agent {
			t.Fatalf("row = %v", row)
		}
		got = append(got, fmt.Sprintf("%v/%v/%v", row["denyRule"], row["denyKind"], row["denySummary"]))
	}
	wantRows := []string{
		"host-pkill/pkill/pkill -f (+1 args)",
		"deployer-signal/pkill/pkill -f tt-deployer",
		"old-site-deploy/npm-run/npm run deploy:static",
		"truenas-tailscale/ssh/ssh truenas tailscale status",
	}
	if strings.Join(got, "\n") != strings.Join(wantRows, "\n") {
		t.Fatalf("rows = %q; want %q", got, wantRows)
	}

	// A call that matches nothing, a post and a tool other than Bash leave
	// no would-deny row.
	count := len(toolLedgerRows(t, root, e.agent))
	toolHook(t, e, "PreToolUse", bashPre("u3", "ls -la"))
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u4", "tool_name": "Read", "tool_input": map[string]any{"file_path": "/repo/scripts/deploy-static.mjs", "command": "pkill node"}})
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u5", "tool_name": "Write", "tool_input": map[string]any{"file_path": "/repo/run.sh", "content": "pkill node\nnpm run deploy:static\n"}})
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u6", "tool_name": "Bash", "tool_input": map[string]any{"command": 7}})
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u7", "tool_name": "Bash", "tool_input": "pkill node"})
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u8", "tool_name": "Bash"})
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != count {
		t.Fatalf("rows from calls that match nothing = %v", rows[count:])
	}
	toolHook(t, e, "PostToolUse", bashPre("u9", "killall node"))
	toolHook(t, e, "PostToolUseFailure", bashPre("u10", "killall node"))
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != count+2 || len(wouldDenyRows(rows[count:])) != 0 {
		t.Fatalf("rows from a post and a failure = %v", rows[count:])
	}
}

// a2: the summary is built from fixed words and a count. Nothing typed in
// the command reaches any file: not a secret with a known shape and not one
// without.
func TestToolDenySummary(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)   // the summaries are the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	e.token = "hub-token-0123456789abcdefghij"
	github := "ghp_" + strings.Repeat("a1B2", 9)
	secrets := []string{
		e.token, github, "ab12cd34", "zzQ-not-a-shape", "hunter2-positional", "assigned-secret-value",
		"go test", "truenas_admin", "truenas-tailarr", "BatchMode", "midclt", "/srv/site", "pane_pid", "list-panes",
	}
	cases := []struct {
		command string
		want    []string // rule: summary
	}{
		{`pkill -f ab12cd34`, []string{"host-pkill: pkill -f (+1 args)"}},
		{`ssh truenas tailscale up --authkey=zzQ-not-a-shape`, []string{"truenas-tailscale: ssh truenas tailscale up (+1 args)"}},
		{`node scripts/deploy-static.mjs hunter2-positional`, []string{"old-site-deploy: node deploy-static.mjs (+1 args)"}},
		{`node scripts/deploy-static.mjs --check`, []string{"old-site-deploy: node deploy-static.mjs (+1 args)"}},
		{`TOKEN=assigned-secret-value pkill -9 -f "go test"`, []string{"host-pkill: pkill -9 -f (+1 args)"}},
		{`pkill -f ` + github, []string{"host-pkill: pkill -f (+1 args)"}},
		{`killall -SIGKILL -q ` + e.token, []string{"host-pkill: killall -KILL -q (+1 args)"}},
		{`sudo /usr/bin/pkill -fx --newest tmux`, []string{"host-pkill: pkill -f -x (+2 args)"}},
		{`tmux kill-session -t tt-deployer-compact`, []string{"deployer-signal: tmux kill-session -t tt-deployer-compact"}},
		{`tmux send-keys -t tt-deployer-11ee78b6de77e2da C-c`, []string{"deployer-signal: tmux send-keys -t tt-deployer-11ee78b6de77e2da (+1 args)"}},
		{`tmux -L main send-keys -t tt-deployer-compact:0.0 "echo ` + github + `" Enter`, []string{"deployer-signal: tmux send-keys -t tt-deployer (+5 args)"}},
		{`kill $(tmux list-panes -t tt-deployer-compact -F '#{pane_pid}')`, []string{"deployer-signal: kill tt-deployer (+1 args)"}},
		{`kill -TERM -- tt-deployer-` + e.token, []string{"deployer-signal: kill -TERM tt-deployer (+2 args)"}},
		{`npm --prefix /srv/site run-script deploy:static -- --token ` + github, []string{"old-site-deploy: npm --prefix run-script deploy:static (+4 args)"}},
		{`/srv/site/scripts/deploy-static.mjs --key ` + e.token, []string{"old-site-deploy: deploy-static.mjs (+2 args)"}},
		{`ssh truenas_admin@truenas-tailarr 'sudo tailscale up --ssh'`, []string{"truenas-tailscale: ssh truenas tailscale up (+2 args)"}},
		{`ssh -o BatchMode=yes truenas "midclt call app.query; systemctl restart tailscaled"`, []string{"truenas-tailscale: ssh -o truenas tailscale (+7 args)"}},
		{`ssh -i /srv/site/id -p 2222 truenas_admin@100.116.238.37 tailscale status`, []string{"truenas-tailscale: ssh -i -p 100.116.238.37 tailscale status (+2 args)"}},
		{`pkill ` + strings.Repeat("-f ", 40) + `node`, []string{"host-pkill: pkill -f -f -f -f -f -f -f (+34 args)"}},
		{"bash <<'EOF'\npkill -f ab12cd34\nEOF", []string{"host-pkill: pkill -f (+1 args)"}},
		{"ssh truenas <<'EOF'\nsudo tailscale up --authkey=zzQ-not-a-shape\nEOF", []string{"truenas-tailscale: ssh truenas tailscale up (+2 args)"}},
		{`nice -n 5 pkill -f ab12cd34`, []string{"host-pkill: pkill -f (+1 args)"}},
		{`yarn deploy:static --token ` + github, []string{"old-site-deploy: yarn deploy:static (+2 args)"}},
		{`tmux new -d -s ab12cd34 \; kill-session -t tt-deployer-compact`, []string{"deployer-signal: tmux kill-session -t tt-deployer-compact (+5 args)"}},
		{"(( n << 2 ))\npkill -f ab12cd34", []string{"host-pkill: pkill -f (+1 args)"}},
		{`files=(scripts/deploy-static.mjs ab12cd34); case $x in pkill|killall) tmux send-keys -t tt-builder-1 'tt-deployer-compact' ;; esac; [[ $x =~ (pkill|killall) ]]; pkill --help; pkill -0 -f vite; killall -d node`, nil},
	}
	for i, c := range cases {
		id := fmt.Sprintf("u%d", i)
		toolHook(t, e, "PreToolUse", bashPre(id, c.command))
		var got []string
		for _, row := range wouldDenyRows(toolLedgerRows(t, root, e.agent)) {
			if row["toolUseId"] == id {
				got = append(got, fmt.Sprintf("%v: %v", row["denyRule"], row["denySummary"]))
			}
		}
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s\n got %q\nwant %q", c.command, got, c.want)
		}
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range secrets {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s holds %q from a command line", path, secret)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The assembled text still passes through the secret scan.
	far := time.Now().Add(time.Minute)
	if got := toolDenySummary([]string{"pkill", github, e.token}, toolRedactExact(e.token), far); got != "pkill [redacted] [redacted]" {
		t.Errorf("scanned summary = %q", got)
	}
	if got := toolDenySummary([]string{"pkill", "-f"}, nil, time.Now().Add(-time.Second)); got != "" {
		t.Errorf("summary after an unfinished scan = %q; want none", got)
	}
	if got := toolDenySummary([]string{strings.Repeat("é", 100)}, nil, far); len(got) != toolDenySummaryBytes || !strings.HasPrefix(strings.Repeat("é", 100), got) {
		t.Errorf("long summary = %d bytes %q", len(got), got)
	}
}

// a3: a check that cannot finish records nothing extra, and the call's
// pending entry and later row are as they would be without it.
func TestToolDenyFailsOpen(t *testing.T) {
	// What each failure leaves in the ledger is the subject here, not timing:
	// a hook that reaches its deadline on a loaded host writes no row.
	// The deadline passed case sets its own deadlines.
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	pending := func(t *testing.T, e env, root string) {
		t.Helper()
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != 0 {
			t.Fatalf("rows = %v; want none", rows)
		}
		if files := toolPendingFiles(t, root, e.agent); len(files) != 1 {
			t.Fatalf("pending files = %d; want 1", len(files))
		}
		toolHook(t, e, "PostToolUse", bashPre("u1", "pkill node"))
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != 1 || rows[0]["outcome"] != "ok" || rows[0]["durationSource"] != "measured" {
			t.Fatalf("rows after the post = %v", rows)
		}
	}
	t.Run("check panics", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		setToolLedger(t, &toolDenyCheck, func(toolLedger, toolLedgerRow, string, json.RawMessage) { panic("classifier on fire") })
		toolHook(t, e, "PreToolUse", bashPre("u1", "pkill node"))
		pending(t, e, root)
	})
	t.Run("write panics inside the check", func(t *testing.T) {
		_, root := toolLedgerSandbox(t)
		l := toolLedger{dir: filepath.Join(root, "agt_ledger"), agent: "agt_ledger", started: time.Now(), deadline: time.Minute, denyMax: toolDenyMaxCommand, redactBudget: time.Minute}
		l.wouldDeny(toolLedgerRow{}, "Bash", json.RawMessage(`{"command":"pkill node"}`)) // l.now is nil
		requireEmptyRoot(t, root)
	})
	t.Run("command over the cap", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		setToolLedger(t, &toolDenyMaxCommand, 64)
		toolHook(t, e, "PreToolUse", bashPre("u1", "pkill node; echo "+strings.Repeat("x", 64)))
		pending(t, e, root)
	})
	t.Run("command at the cap", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		command := "pkill node; echo "
		setToolLedger(t, &toolDenyMaxCommand, 64)
		toolHook(t, e, "PreToolUse", bashPre("u1", command+strings.Repeat("x", 64-len(command))))
		if rows := toolLedgerRows(t, root, e.agent); len(wouldDenyRows(rows)) != 1 {
			t.Fatalf("rows = %v; want the would-deny row", rows)
		}
	})
	t.Run("deadline passed", func(t *testing.T) {
		// On a loaded host the hook can reach its 40 ms deadline before the
		// check is called, and then it never is: that attempt shows nothing
		// about a late check, so it is run again in a new sandbox, at most
		// three times. The last attempt is judged as it stands.
		var e env
		var root string
		for attempt := 1; ; attempt++ {
			e, root = toolLedgerSandbox(t)
			setToolLedger(t, &toolLedgerDeadline, 40*time.Millisecond)
			called, finished := make(chan struct{}), make(chan struct{})
			setToolLedger(t, &toolDenyCheck, func(l toolLedger, row toolLedgerRow, tool string, input json.RawMessage) {
				close(called)
				defer close(finished)
				time.Sleep(time.Until(l.started.Add(l.deadline + 20*time.Millisecond)))
				l.wouldDeny(row, tool, input)
			})
			toolHook(t, e, "PreToolUse", bashPre("u1", "pkill node"))
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				select {
				case <-called:
				default:
					if attempt < 3 {
						t.Logf("attempt %d: the hook reached its %v deadline before the check was called; running it again in a new sandbox", attempt, toolLedgerDeadline)
						continue
					}
				}
				t.Fatal("the late check never finished")
			}
			break
		}
		setToolLedger(t, &toolLedgerDeadline, 30*time.Second) // the post hook's row is not about timing: a hook that reaches its deadline on a loaded host writes no row
		pending(t, e, root)
	})
	t.Run("unreadable input", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		pre := toolPayload("PreToolUse", bashPre("u1", "pkill node"))
		for _, payload := range []string{pre[:len(pre)-1], "", `{"hook_event_name":"Nope","tool_name":"Bash","tool_input":{"command":"pkill node"}}`} {
			if r := runToolHook(t, e, "tool", payload, nil); r.err != nil || r.out != "" {
				t.Fatalf("hook = %v, output %q", r.err, r.out)
			}
		}
		setToolLedger(t, &toolLedgerMaxInput, 256)
		big := bashPre("u2", "pkill node")
		big["description"] = strings.Repeat("d", 512)
		toolHook(t, e, "PreToolUse", big)
		rows := toolLedgerRows(t, root, e.agent)
		if len(rows) != 4 {
			t.Fatalf("rows = %v; want four", rows)
		}
		for _, row := range rows {
			if row["outcome"] != "unreadable" || row["denyRule"] != nil {
				t.Fatalf("row = %v; want unreadable only", row)
			}
		}
	})
}

// a3: matching and non-matching calls alike return nil and print nothing,
// on all three events.
func TestToolDenyPrintsNothing(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)   // silence and the row count are the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	for i, command := range []string{"ls", "pkill node", "pkill -f tt-deployer; npm run deploy:static; ssh truenas tailscale up"} {
		for _, event := range []string{"PreToolUse", "PostToolUse", "PreToolUse", "PostToolUseFailure"} {
			if r := runToolHook(t, e, "tool", toolPayload(event, bashPre(fmt.Sprintf("u%d", i), command)), nil); r.err != nil || r.out != "" {
				t.Fatalf("%s %q = %v, output %q; want nil and no output", event, command, r.err, r.out)
			}
		}
	}
	if got := len(wouldDenyRows(toolLedgerRows(t, root, e.agent))); got != 2*(0+1+4) {
		t.Fatalf("would-deny rows = %d; want 10", got)
	}
}

func toolDenyMedian(d []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func toolDenyCapCommand(head string) string {
	filler := `echo "a quoted pkill node; ssh truenas tailscale up" 'npm run deploy:static' $(date) | grep -c x && true; `
	return head + strings.Repeat(filler, (toolDenyMaxCommand-len(head))/len(filler))
}

// toolDenyKeywordCommand is one simple command of keyword words and then
// unit repeated, about 60 KiB in all, and after it a command that matches.
func toolDenyKeywordCommand(unit string) string {
	return strings.Repeat("! ", 15000) + strings.Repeat(unit, 30000/len(unit)) + "\npkill node"
}

const toolDenyAllRules = "pkill -f tt-deployer; npm run deploy:static; ssh truenas tailscale status"

// a6: the added cost of the check per PreToolUse, measured against the same
// hook with the check replaced by nothing. Every run with the check that
// returns before the hook's deadline has its would-deny rows already written.
// A run that reached the deadline is put down to a stalled host and run
// again, at most three times, and a case allows ten such attempts in all.
//
// The 5 ms limit is for the binary that ships. The race detector slows the
// JSON decode of a 64 KiB command about tenfold, so under it the limit is
// 25 ms; the deadline and the rows are checked the same either way.
func TestToolDenyCost(t *testing.T) {
	const runs = 100
	limit := 5 * time.Millisecond
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "-race" && setting.Value == "true" {
				limit = 25 * time.Millisecond
				t.Logf("race detector on: the added-cost limit is %v", limit)
			}
		}
	}
	e, root := toolLedgerSandbox(t)
	real := toolDenyCheck
	cases := []struct {
		name, command string
		rows          int
	}{
		{"no match, short", "go test ./cmd/tt -run TestToolDeny -count=1", 0},
		{"one rule", "pkill node", 1},
		{"four rules", toolDenyAllRules, 4},
		{"one rule at the 64 KiB cap", toolDenyCapCommand("pkill node; "), 1},
		{"no match at the 64 KiB cap", toolDenyCapCommand(""), 0},
		// m1: 15,000 keyword words, then a case or a [[ ]] at every third
		// word, about 60 KiB. Reading the words again at each one cost 30 ms.
		{"keywords then repeated case", toolDenyKeywordCommand("case a b "), 1},
		{"keywords then repeated [[ ]]", toolDenyKeywordCommand("[[ ]] "), 1},
	}
	for n, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var with, without []time.Duration
			late := 0
			var worst time.Duration
			// The rows are promised to a hook that ends inside the deadline. A
			// run that reached it may fail open with none (docs/claude-wake.md),
			// so it is put down to a stalled host and run again as a new agent,
			// as TestToolScopeCost does. Stalls come in bursts, so each new
			// attempt waits first. A check that is itself slow must still fail,
			// so the stalled attempts of a case are capped as well.
			const attempts, maxStalled = 3, 10
			for i := 0; i < 2*runs; i++ {
				// Alternate, and swap which goes first each pair.
				check := (i+i/2)%2 == 0
				for attempt := 1; ; attempt++ {
					toolDenyCheck = nil
					if check {
						toolDenyCheck = real
					}
					e.agent = fmt.Sprintf("agt_cost_%d_%d_%d", n, i, attempt)
					r := runToolHook(t, e, "tool", toolPayload("PreToolUse", bashPre("u1", c.command)), nil)
					toolDenyCheck = real
					if r.err != nil || r.out != "" {
						t.Fatalf("hook = %v, output %q", r.err, r.out)
					}
					rows := len(wouldDenyRows(toolLedgerRows(t, root, e.agent)))
					switch {
					case !check:
						if rows != 0 {
							t.Fatalf("baseline run wrote %d would-deny rows", rows)
						}
						without = append(without, r.elapsed)
					case r.elapsed >= toolLedgerDeadline:
						late++
						t.Logf("run %d attempt %d reached the %v deadline in %v with %d of %d rows", i, attempt, toolLedgerDeadline, r.elapsed, rows, c.rows)
						if attempt == attempts {
							t.Fatalf("run %d reached the %v deadline %d times in a row; a stalled host does not explain that", i, toolLedgerDeadline, attempts)
						}
						if late > maxStalled {
							t.Fatalf("%d attempts in %d runs reached the hook deadline; at most %d are put down to a stalled host", late, runs, maxStalled)
						}
						time.Sleep(time.Duration(attempt) * time.Second)
						continue
					default:
						if rows != c.rows {
							t.Fatalf("run %d returned in %v with %d would-deny rows; want %d", i, r.elapsed, rows, c.rows)
						}
						with = append(with, r.elapsed)
						worst = max(worst, r.elapsed)
					}
					break
				}
			}
			if late > 0 {
				t.Logf("%d attempt(s) with the check were put down to a stalled host and run again; the cap is %d", late, maxStalled)
			}
			base, checked := toolDenyMedian(without), toolDenyMedian(with)
			t.Logf("%d bytes: baseline median %v, with the check %v, added %v, worst run with the check %v", len(c.command), base, checked, checked-base, worst)
			if checked-base >= limit {
				t.Fatalf("the check adds %v to the median hook; want under %v", checked-base, limit)
			}
		})
	}
}

// a6: a matching pre still returns nil and silent inside the hook's bound.
func TestToolDenyReturnBound(t *testing.T) {
	for name, command := range map[string]string{"one rule": "pkill node", "four rules": toolDenyAllRules, "64 KiB": toolDenyCapCommand("pkill node; ")} {
		t.Run(name, func(t *testing.T) {
			e, _ := toolLedgerSandbox(t)
			var elapsed time.Duration
			for attempt := 0; attempt < 3; attempt++ {
				time.Sleep(time.Duration(attempt) * toolLedgerBound)
				r := runToolHook(t, e, "tool", toolPayload("PreToolUse", bashPre(fmt.Sprintf("u%d", attempt), command)), nil)
				if r.err != nil || r.out != "" {
					t.Fatalf("hook = %v, output %q; want nil and no output", r.err, r.out)
				}
				if elapsed = r.elapsed; elapsed < toolLedgerBound {
					return
				}
			}
			t.Fatalf("hook took %v three times; want under %v", elapsed, toolLedgerBound)
		})
	}
}

// a4: the command docs/claude-wake.md gives counts would-deny rows by rule
// and agent, across agents and a rotated file, past a torn last line.
func TestToolDenyDocCommand(t *testing.T) {
	const jq = "/usr/bin/jq"
	if _, err := os.Stat(jq); err != nil {
		t.Skipf("%s is not installed", jq)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "claude-wake.md"))
	if err != nil {
		t.Fatal(err)
	}
	command := ""
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.Contains(line, `select(.outcome == "would_deny")`) {
			if command != "" {
				t.Fatal("docs/claude-wake.md gives the would-deny summary command more than once")
			}
			command = line
		}
	}
	if !strings.HasPrefix(command, "awk 1 ~/.local/state/tailterm/tool-ledger/") || !strings.Contains(command, jq) {
		t.Fatalf("docs/claude-wake.md does not give the would-deny summary command: %q", command)
	}
	home := t.TempDir()
	write := func(agent, name string, lines ...string) {
		dir := filepath.Join(home, ".local", "state", "tailterm", "tool-ledger", agent)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	row := func(agent, outcome, rule string) string {
		b, _ := json.Marshal(toolLedgerRow{V: 1, Agent: agent, Tool: "Bash", Outcome: outcome, DenyRule: rule})
		return string(b)
	}
	write("agt_a", "ledger.jsonl.1", row("agt_a", "would_deny", "host-pkill"), row("agt_a", "ok", ""), "")
	write("agt_a", "ledger.jsonl", row("agt_a", "would_deny", "host-pkill"), row("agt_a", "would_deny", "old-site-deploy"), row("agt_a", "unknown", ""), `{"v":1,"agent":"agt_a","outcome":"would_de`)
	write("agt_b", "ledger.jsonl", row("agt_b", "ok", ""), row("agt_b", "would_deny", "truenas-tailscale"), "")
	write("agt_c", "ledger.jsonl", row("agt_c", "ok", ""), "")
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", command, err, out)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	want := []string{"2 host-pkill agt_a", "1 old-site-deploy agt_a", "1 truenas-tailscale agt_b"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary = %q; want %q", got, want)
	}
}

// The handler-gate report (feature wi_cbc08bd2b2a419a0). As above, every
// command is text for the classifier or the hook: none is run, no hub is
// asked and no database is opened.

const (
	toolGateTestHub  = "http://gatehub.test:8443"
	toolGateAllRules = "tt work-items create --title x; curl -X POST http://gatehub.test:8443/api/x; sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db .tables"
)

// gateSandbox is toolLedgerSandbox with a named agent and a hub address the
// tests choose, whatever the session running them has in its environment.
func gateSandbox(t *testing.T) (env, string) {
	t.Helper()
	e, root := toolLedgerSandbox(t)
	t.Setenv("TAILTERM_HUB", toolGateTestHub)
	e.agentName = "builder-x"
	return e, root
}

func gateRuleNames(command, hub string) string {
	_, gates := toolGateClassify(command, hub, true)
	return denyRuleNames(gates)
}

func gateRowTexts(rows []map[string]any) []string {
	var out []string
	for _, row := range wouldReportRows(rows) {
		out = append(out, fmt.Sprintf("%v/%v/%v", row["reportRule"], row["reportTag"], row["reportSummary"]))
	}
	return out
}

const toolGateDoneSave = "tt work-items update wi_0123456789abcdef --status done --worktree /w/queue-x --branch fix-x --commit 0123abc"

// k5: the same save with the single-dash flags Go's flag package accepts.
const toolGateDoneSaveOneDash = "tt work-items update wi_0123456789abcdef -status done -worktree /w/queue-x -branch fix-x -commit 0123abc"

// toolGateWriteForms is every write form of the plan's table.
func toolGateWriteForms() []string {
	forms := []string{
		"tt work-items create --kind bug --title x",
		"tt work-items update wi_1 --status open",
		"tt work-items dispatch wi_1 --to lead-x",
		"tt work-items note wi_1 --text x",
		"tt work-items evidence wi_1",
		"tt work-items scope confirm wi_1 --owns a.go",
		"tt work-items scope save wi_1 --owns a.go",
		"tt work-items narrative artifact-put wi_1 --file f",
		"tt work-items narrative link-put wi_1",
		"tt work-items narrative coverage-put wi_1",
		"tt work-items narrative report-put wi_1",
		"tt work-items triage set wi_1 --lane small",
		"tt work-items triage confirm wi_1",
		"tt queue action pull --item wi_1",
		"tt team queue wait set --entry tqe_1 --on tqe_2",
		"tt team queue wait clear --entry tqe_1 --on tqe_2",
		"tt verification plan --item wi_1 --file plan.json",
		"tt verification receipt --item wi_1 --check v1",
		toolGateDoneSave,
		"tt work-items update wi_1 --status=done --worktree=/w --branch=b --commit=c",
	}
	for _, sub := range strings.Fields("add policy limit provision scope rebind requeue fail accept integrated replace-lead remove reorder release abandon") {
		forms = append(forms, "tt team queue "+sub+" --entry tqe_1")
	}
	return forms
}

// a1, a2, a3: every rule fires on its write forms, alone and wrapped, and
// on none of its read forms and near-misses.
func TestToolGateClassify(t *testing.T) {
	for _, form := range toolGateWriteForms() {
		for _, command := range []string{
			form, "sudo " + form, "/Users/x/.local/bin/" + form, "bash -c '" + form + "'", `eval "` + form + `"`,
			"ssh air '" + form + "'", "echo $(" + form + ")", "cd /w && FOO=1 timeout 30 " + form + " 2>&1 | tail -3",
		} {
			if got := gateRuleNames(command, toolGateTestHub); got != toolGateWorkItem {
				t.Errorf("%s: rules = %q; want %s", command, got, toolGateWorkItem)
			}
			if got := toolDenyClassify(command); len(got) != 0 {
				t.Errorf("%s: deny rules = %q; want none", command, denyRuleNames(got))
			}
		}
	}
	const hub, port = "gatehub.test:8443", "other.test:18765"
	fires := []struct{ rule, hub, command string }{
		{toolGateHubWrite, toolGateTestHub, `curl -X POST http://` + hub + `/api/tasks`},
		{toolGateHubWrite, toolGateTestHub, `curl -XPUT http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl --request=DELETE http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl --request DELETE http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -d a=b http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl --data-binary @f http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl --json '{"a":1}' http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -F file=@f http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -T f http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -I http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl --head http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -sS -H "Authorization: Bearer x" -X POST ` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -X POST http://user:pw@` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -X POST HTTP://GATEHUB.test:8443`},
		{toolGateHubWrite, toolGateTestHub, `curl -X POST --url http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -o out.json -X PATCH https://example.com/a http://` + hub + `/b`},
		{toolGateHubWrite, toolGateTestHub, `wget --post-data a=b http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `wget --post-file f http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `wget --method=PUT --body-file=f http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `wget -q --method DELETE http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `http POST ` + hub + `/api/x a=b`},
		{toolGateHubWrite, toolGateTestHub, `http ` + hub + `/api/x count:=3`},
		{toolGateHubWrite, toolGateTestHub, `xh PUT http://` + hub + `/api/x`},
		// The hub's port on another host, with TAILTERM_HUB set, unset and on its scheme's port.
		{toolGateHubWrite, toolGateTestHub, `curl -sSXPOST http://` + port + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -d x 100.64.0.9:18765`},
		{toolGateHubWrite, toolGateTestHub, `curl -X POST 'http://[fd7a::1]:18765/api/x'`},
		{toolGateHubWrite, "", `curl -X POST http://` + port + `/api/x`},
		{toolGateHubWrite, "", `wget --method=PUT http://` + port + `/api/x`},
		{toolGateHubWrite, "", `http POST :18765/api/x`},
		{toolGateHubWrite, "", `https DELETE ` + port + `/api/x`},
		{toolGateHubWrite, "https://tailhub.test", `curl -X POST https://tailhub.test/api/x`},
		{toolGateHubWrite, "https://tailhub.test", `https POST tailhub.test/api/x`},
		// The variable as written, in the authority position.
		{toolGateHubWrite, "", `curl -X POST $TAILTERM_HUB/api/x`},
		{toolGateHubWrite, "", `curl -X POST "${TAILTERM_HUB}/api/x"`},
		{toolGateHubWrite, "", `curl -d x $TAILTERM_HUB`},
		{toolGateHubWrite, "", `curl -X POST "$TAILTERM_HUB?x=1"`},
		{toolGateHubWrite, toolGateTestHub, `ssh air 'curl -X POST http://127.0.0.1:18765/api/x'`},
		{toolGateHubWrite, toolGateTestHub, `bash -c "curl -X POST http://127.0.0.1:18765/api/x"`},
		{toolGateHubWrite, toolGateTestHub, `TOKEN=$(cat t) && sudo curl -X POST http://` + hub + `/x`},
		// k5: HTTPie posts what it is fed, a --raw body, and any method but GET.
		{toolGateHubWrite, toolGateTestHub, `echo '{}' | http ` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `cat body.json | sudo xh $TAILTERM_HUB/api/x`},
		{toolGateHubWrite, toolGateTestHub, `http ` + hub + `/api/x < body.json`},
		{toolGateHubWrite, toolGateTestHub, "http " + hub + "/api/x <<'EOF'\n{}\nEOF"},
		{toolGateHubWrite, toolGateTestHub, `http ` + hub + `/api/x <<< '{}'`},
		{toolGateHubWrite, "", `http --raw '{}' :18765/x`},
		{toolGateHubWrite, "", `http --raw='{}' :18765/x`},
		{toolGateHubWrite, "", `http PURGE :18765/x`},
		{toolGateHubWrite, toolGateTestHub, `xh purge ` + hub + `/x`},
		// k5: curl's --expand-url is a URL, a strip of the variable is still the hub, and --next starts a new request.
		{toolGateHubWrite, toolGateTestHub, `curl -X POST --expand-url http://` + hub + `/api/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -d x --expand-url=http://` + hub + `/api/{{name}}`},
		{toolGateHubWrite, "", `curl -X POST "${TAILTERM_HUB%/}/api/x"`},
		{toolGateHubWrite, "", `curl -d x ${TAILTERM_HUB%%/}`},
		{toolGateHubWrite, "", `curl -d x "${TAILTERM_HUB#http://}/api/x"`},
		{toolGateHubWrite, toolGateTestHub, `curl https://example.com --next -X POST http://` + hub + `/x`},
		{toolGateHubWrite, toolGateTestHub, `curl -d x http://` + hub + `/x -: https://example.com`},

		{toolGateHubDB, "", `sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db`},
		{toolGateHubDB, "", `sqlite3 /mnt/deepfreeze/tailterm-hub`},
		{toolGateHubDB, "", `sqlite3 -readonly /mnt/deepfreeze/tailterm-hub/hub.db .tables`},
		{toolGateHubDB, "", `sqlite3 'file:/mnt/deepfreeze/tailterm-hub/hub.db?mode=ro'`},
		{toolGateHubDB, "", `sqlite3 file:///mnt/deepfreeze/tailterm-hub/hub.db .tables`},
		{toolGateHubDB, "", `sqlite3 'file://localhost/mnt/deepfreeze/tailterm-hub/hub.db?mode=ro'`},
		{toolGateHubDB, "", `sqlite3 -cmd '.timeout 5' /mnt/deepfreeze/tailterm-hub/hub.db`},
		{toolGateHubDB, "", `sqlite3 -header -separator , /mnt/deepfreeze/tailterm-hub/data/hub.db "select 1"`},
		{toolGateHubDB, "", `ssh truenas 'sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db .tables'`},
		{toolGateHubDB, "", `ssh truenas sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db .tables`},
		{toolGateHubDB, "", `sudo /usr/bin/sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db`},

		{"work-item-write,hub-direct-write,hub-db-direct", toolGateTestHub, toolGateAllRules},
		{"work-item-write,hub-direct-write,hub-db-direct", toolGateTestHub, "sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db; curl -d x :18765; tt queue action pull"},
	}
	for _, c := range fires {
		if got := gateRuleNames(c.command, c.hub); got != c.rule {
			t.Errorf("%s (hub %q): rules = %q; want %s", c.command, c.hub, got, c.rule)
		}
	}
	nearMisses := []string{
		// Reads and other tt commands.
		"tt work-items list --status open", "tt work-items get wi_1", "tt work-items receipt wir_1", "tt work-items revisions wi_1",
		"tt work-items messages wi_1", "tt work-items triage --json", "tt work-items triage", "tt work-items scope get wi_1",
		"tt work-items scope receipt wir_1", "tt work-items narrative overview wi_1", "tt work-items narrative timeline wi_1",
		"tt work-items narrative artifacts wi_1", "tt work-items narrative artifact-versions wi_1", "tt work-items narrative artifact-get wi_1",
		"tt work-items narrative links wi_1", "tt work-items narrative coverage wi_1", "tt work-items narrative reports wi_1",
		"tt work-items narrative report-get wi_1", "tt work-items narrative receipt wir_1",
		"tt queue list", "tt queue get wi_1", "tt queue history wi_1", "tt queue changes", "tt queue receipt qr_1",
		"tt team queue list", "tt team queue sweep-worktrees", "tt team launch --template small", "tt team queue wait", "tt team queue wait show",
		"tt verification history --item wi_1", "tt verification enrollment",
		"tt send --kind result --subject 'tt work-items create'", `tt post "tt work-items update wi_1"`, "tt ack 7", "tt inbox --unread --mark-read",
		"tt status", "tt", "tt work-items", "tt work-items --help", "tt work-items bogus create", "tt --task x work-items create",
		"tt team queue", "tt queue", "tt verification", "ttx work-items create", "echo tt work-items create", `echo "tt work-items create"`,
		"command -v tt", `git commit -m "tt work-items update wi_1 --status done"`, "grep -n 'tt queue action' docs/handoff.md",
		"cat > notes.txt <<'EOF'\ntt work-items create\ncurl -X POST http://h:18765/x\nsqlite3 /mnt/deepfreeze/tailterm-hub/hub.db\nEOF\necho done",
		// A GET, another host or port, and the hub named anywhere but a URL's authority.
		`curl $TAILTERM_HUB/api/tasks`, `curl http://` + hub + `/api/tasks`, `curl -sS -H "Authorization: Bearer x" http://` + port + `/api/x`,
		`curl -X GET http://` + port + `/api/x`, `curl -G -d a=b http://` + port + `/api/x`, `curl --get --data-urlencode q=x http://` + hub + `/x`,
		`curl -X POST https://example.com/x`, `curl -X POST host:8765`, `curl -X POST http://gatehub.test:8444/x`, `curl -X POST http://gatehub.test/x`,
		`curl -X POST https://` + hub[:len(hub)-5] + `/x`,
		`curl -H 'X: http://h:18765' https://example.com -d x`, `curl -H "Referer: http://` + hub + `/" -X POST https://example.com`,
		`curl -X POST 'https://example.com/x?next=http://h:18765/y'`, `curl -X POST https://example.com/18765`,
		`curl -X POST https://example.com/p:18765/x`, `curl -X POST https://example.com/$TAILTERM_HUB`,
		`curl -X POST "https://example.com/?u=$TAILTERM_HUB"`, `curl -X POST https://example.com/#:18765`,
		`curl -X POST $TAILTERM_HUB_OLD/x`, `curl -X POST ${TAILTERM_HUBS}/x`, `curl -d 'http://h:18765' https://example.com`,
		`curl -d @http://h:18765 -o h:18765 -u u:18765 https://example.com`, `curl --data=x --proxy http://p:18765 https://example.com`,
		`wget http://h:18765/x`, `wget -O out http://` + hub + `/x`, `wget --method=GET http://h:18765/x`,
		`wget --header 'X: http://h:18765' --post-data a=b https://example.com`,
		`http GET ` + hub + `/api/x`, `http ` + hub + `/api/x q==1 X-Trace:on`, `http POST example.com Host:h:18765 next=http://h:18765`,
		`xh ` + port + `/api/x`, `echo curl -X POST http://h:18765`, `echo "curl -X POST http://h:18765/x"`,
		// k5: the write and the hub in different requests of one curl, input that is not fed or not read, and other expansions.
		`curl -X POST https://example.com --next http://` + port + `/x`, `curl -d x https://example.com -: http://` + hub + `/x`,
		`curl http://` + hub + `/x --next -I https://example.com`, `curl -X POST --expand-url https://example.com/x`,
		`echo '{}' | http --ignore-stdin ` + hub + `/api/x`, `echo '{}' | xh -I ` + hub + `/api/x`, `http ` + hub + `/api/x | jq .`,
		`true || http ` + hub + `/api/x`, `echo '{}' | curl http://` + hub + `/api/x`, `echo '{}' | tee b.json; http ` + hub + `/api/x`,
		`http ` + hub + `/api/x > out.json`, `http ` + hub + `/api/x 2< body.json`, `http <(echo x) ` + hub, `http get ` + hub + `/api/x`,
		`http --raw '{}' example.com/x`, `http PURGE example.com/x`, `http localhost q==1`,
		`curl -X POST ${TAILTERM_HUBS%/}/x`, `curl -X POST "${TAILTERM_HUB%:*}:9/x"`, `curl -X POST ${TAILTERM_HUB:-https://example.com}/x`,
		`curl -X POST ${TAILTERM_HUB/8443/9}/x`, `curl -X POST https://example.com/${TAILTERM_HUB%/}`,
		// Another file, a sibling name, and the directory only in SQL text or an option's value.
		`sqlite3 /tmp/x.db`, `sqlite3 ./hub.db`, `sqlite3`, `sqlite3 -readonly`, `ls /mnt/deepfreeze/tailterm-hub`,
		`echo sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db`, `sqlite3 /mnt/deepfreeze/tailterm-hub-backup/hub.db`,
		`sqlite3 /mnt/deepfreeze/tailterm-hubx.db`, `sqlite3 file://otherhost/mnt/deepfreeze/tailterm-hub/hub.db`,
		`sqlite3 file:///tmp/x.db`, `sqlite3 //mnt/deepfreeze/tailterm-hub/hub.db`, `sqlite3 file://localhost`,
		`sqlite3 /tmp/x.db "ATTACH '/mnt/deepfreeze/tailterm-hub/hub.db' AS h"`,
		`sqlite3 /tmp/x.db "select '/mnt/deepfreeze/tailterm-hub/hub.db'"`, `sqlite3 -cmd '/mnt/deepfreeze/tailterm-hub/hub.db' /tmp/x.db`,
		`sqlite3 /tmp/x.db /mnt/deepfreeze/tailterm-hub/hub.db`, `ssh truenas ls -la /mnt/deepfreeze/tailterm-hub/hub.db`,
	}
	for _, command := range nearMisses {
		if got := gateRuleNames(command, toolGateTestHub); got != "" {
			t.Errorf("near-miss %s: rules = %q; want none", command, got)
		}
	}
	// The hub address is the TAILTERM_HUB value: without it only the port is known.
	if got := gateRuleNames(`curl -X POST http://`+hub+`/api/x`, ""); got != "" {
		t.Errorf("a write to another address with no hub set: rules = %q; want none", got)
	}
	// With the gate off the same parse returns the deny matches and no others.
	if deny, gates := toolGateClassify("pkill node; "+toolGateAllRules, toolGateTestHub, false); denyRuleNames(deny) != "host-pkill" || len(gates) != 0 {
		t.Errorf("gate off: deny %q, gate %q", denyRuleNames(deny), denyRuleNames(gates))
	}
	if deny, gates := toolGateClassify("pkill node; "+toolGateAllRules, toolGateTestHub, true); denyRuleNames(deny) != "host-pkill" || len(gates) != 3 {
		t.Errorf("gate on: deny %q, gate %q", denyRuleNames(deny), denyRuleNames(gates))
	}

	summaries := map[string]string{
		"tt work-items scope confirm wi_1 --owns a.go --owns b.go --criteria g1 --order 7": "tt work-items scope confirm (+9 args)",
		"tt team queue wait set --entry tqe_1 --on tqe_2 --until released":                 "tt team queue wait set (+6 args)",
		"tt work-items evidence wi_1": "tt work-items evidence (+1 args)",
		"tt verification plan wi_1":   "tt verification plan (+1 args)",
		toolGateDoneSave:              "tt work-items update --status done --worktree --branch --commit (+4 args)",
		"tt work-items update wi_1 --status=done --worktree=/w --branch=b --commit=c":          "tt work-items update --status done --worktree --branch --commit (+1 args)",
		"tt work-items update wi_1 --status done --branch b --commit c":                        "tt work-items update (+7 args)",
		"tt work-items update wi_1 --status open --worktree /w --branch b --commit c":          "tt work-items update (+9 args)",
		`curl -sS -X POST -H "Authorization: Bearer x" -d '{"a":1}' http://` + port + `/api/x`: "curl POST data port-18765 (+7 args)",
		`curl -X PURGE http://` + hub + `/x`:                                                   "curl method hub (+2 args)",
		`curl -I $TAILTERM_HUB/x`:                                                              "curl HEAD hub (+1 args)",
		`curl -d x http://` + hub:                                                              "curl data hub (+2 args)",
		`wget --method=PUT --body-data x http://` + hub:                                        "wget PUT data hub (+3 args)",
		`http POST ` + hub + `/x a=b`:                                                          "http POST data hub (+2 args)",
		`echo '{"a":1}' | http ` + hub + `/x`:                                                  "http data hub",
		`http --raw '{"a":1}' ` + hub + `/x`:                                                   "http data hub (+2 args)",
		`http PURGE ` + hub + `/x`:                                                             "http method hub (+1 args)",
		`curl -sS --expand-url http://` + hub + `/x -X PUT`:                                    "curl PUT hub (+4 args)",
		`curl -d x "${TAILTERM_HUB%/}/api/x"`:                                                  "curl data hub (+2 args)",
		`curl https://example.com --next -I http://` + port + `/x`:                             "curl HEAD port-18765 (+3 args)",
		toolGateDoneSaveOneDash:                                                                "tt work-items update --status done --worktree --branch --commit (+4 args)",
		"tt work-items update wi_1 -status=done -worktree=/w -branch=b --commit=c":             "tt work-items update --status done --worktree --branch --commit (+1 args)",
		"tt work-items update wi_1 -status done -branch b -commit c":                           "tt work-items update (+7 args)",
		"tt team queue scope --entry tqe_1 --owns a.go":                                        "tt team queue scope (+4 args)",
		"tt verification receipt --item wi_1 --check v1":                                       "tt verification receipt (+4 args)",
		`sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db`:                                          "sqlite3 hub-db",
		`sqlite3 -readonly /mnt/deepfreeze/tailterm-hub/hub.db "select 1"`:                     "sqlite3 hub-db (+2 args)",
	}
	for command, want := range summaries {
		_, gates := toolGateClassify(command, toolGateTestHub, true)
		if len(gates) != 1 || strings.Join(gates[0].parts, " ") != want {
			t.Errorf("%s: matches %v; want the summary %q", command, gates, want)
		}
	}
	// One match per rule. A write the lead may make gives way to one it may not, in either order,
	// and the receipt a verifier may save too gives way to both.
	for command, want := range map[string]string{
		"tt verification plan --item wi_1":                                 toolGateLeadForm,
		toolGateDoneSave:                                                   toolGateLeadForm,
		toolGateDoneSaveOneDash:                                            toolGateLeadForm,
		"tt team queue scope --entry tqe_1":                                toolGateLeadForm,
		"tt team queue accept --entry tqe_1":                               "",
		"tt verification receipt --item wi_1":                              toolGateReceiptForm,
		"tt verification receipt --item wi_1; tt verification plan wi_1":   toolGateLeadForm,
		"tt verification plan wi_1; tt verification receipt --item wi_1":   toolGateLeadForm,
		"tt verification receipt --item wi_1; tt work-items create x":      "",
		"tt work-items create x; tt verification receipt --item wi_1":      "",
		"tt work-items update wi_1 --status done":                          "",
		"tt verification plan --item wi_1; tt work-items create --title x": "",
		"tt work-items create --title x; tt verification plan --item wi_1": "",
	} {
		_, gates := toolGateClassify(command, "", true)
		if len(gates) != 1 || gates[0].kind != want {
			t.Errorf("%s: matches %v; want one of kind %q", command, gates, want)
		}
	}
	// No input makes the gate panic, whatever stands around its commands.
	for _, command := range []string{"", `"`, `'`, "`", `$(`, `${`, `\`, `<<`, `(`, `{`, "tt", "tt work-items", "tt team queue wait", "tt work-items update --status", "curl", "curl -X", "curl -", "curl --", "curl --url", "curl -d", "wget --method", "http", "http POST", "xh", "https", "sqlite3", "sqlite3 -cmd", "sqlite3 file:", "curl ://", "curl @", "curl :", "curl ]:18765 -d x", "curl -X POST [", strings.Repeat("(", 5000)} {
		toolGateClassify(command, toolGateTestHub, true)
		for i := range command {
			toolGateClassify(command[i:]+" tt work-items create; curl -X POST :18765; sqlite3 x; "+command[:i], toolGateTestHub, true)
		}
	}
}

// a4: a gate match leaves one would_report row per rule with the rule, the
// tag, the summary and the agent name, beside the call's ordinary row.
func TestToolGateRow(t *testing.T) {
	e, root := gateSandbox(t)
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)   // the rows' fields are the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	const command = "tt work-items create --kind bug --title x"
	toolHook(t, e, "PreToolUse", bashPre("u1", command))
	rows := toolLedgerRows(t, root, e.agent)
	if len(rows) != 1 {
		t.Fatalf("rows after a matching pre = %v; want one", rows)
	}
	want := map[string]any{
		"v": float64(1), "task": e.task, "agent": e.agent, "run": e.runID, "session": "s1", "tool": "Bash", "toolUseId": "u1",
		"argsDigest": toolArgsDigest(json.RawMessage(`{"command":"` + command + `"}`)),
		"outcome":    "would_report", "reportRule": "work-item-write", "reportTag": "none",
		"reportSummary": "tt work-items create (+4 args)", "agentName": "builder-x",
	}
	for k, v := range want {
		if rows[0][k] != v {
			t.Errorf("%s = %v; want %v", k, rows[0][k], v)
		}
	}
	if len(rows[0]) != len(want)+1 || rows[0]["time"] == nil {
		t.Errorf("row fields = %s; want those of %v and time", rowKeys(rows[0]), want)
	}
	if pending := toolPendingFiles(t, root, e.agent); len(pending) != 1 {
		t.Fatalf("pending files = %d; want the call's one", len(pending))
	}
	toolHook(t, e, "PostToolUse", bashPre("u1", command))
	rows = toolLedgerRows(t, root, e.agent)
	if len(rows) != 2 || rows[1]["outcome"] != "ok" || rows[1]["durationSource"] != "measured" || rows[1]["reportRule"] != nil ||
		rows[1]["reportTag"] != nil || rows[1]["reportSummary"] != nil || rows[1]["agentName"] != nil {
		t.Fatalf("rows after the post = %v", rows)
	}

	// One row per rule, in rule order.
	toolHook(t, e, "PreToolUse", bashPre("u2", toolGateAllRules))
	got := gateRowTexts(toolLedgerRows(t, root, e.agent))[1:]
	wantRows := []string{
		"work-item-write/none/tt work-items create (+2 args)",
		"hub-direct-write/none/curl POST hub (+2 args)",
		"hub-db-direct/none/sqlite3 hub-db (+1 args)",
	}
	if strings.Join(got, "\n") != strings.Join(wantRows, "\n") {
		t.Fatalf("rows = %q; want %q", got, wantRows)
	}

	// A deny rule and a gate rule: one row of each, the would-deny row as before.
	count := len(toolLedgerRows(t, root, e.agent))
	toolHook(t, e, "PreToolUse", bashPre("u3", "pkill node; tt queue action pull --item wi_1"))
	rows = toolLedgerRows(t, root, e.agent)[count:]
	if len(rows) != 2 || len(wouldDenyRows(rows)) != 1 || len(wouldReportRows(rows)) != 1 {
		t.Fatalf("rows = %v; want one would_deny and one would_report", rows)
	}
	if rowKeys(rows[0]) != "agent,argsDigest,denyKind,denyRule,denySummary,outcome,run,session,task,time,tool,toolUseId,v" || rows[0]["denySummary"] != "pkill (+1 args)" {
		t.Fatalf("would-deny row = %v", rows[0])
	}
	if rowKeys(rows[1]) != "agent,agentName,argsDigest,outcome,reportRule,reportSummary,reportTag,run,session,task,time,tool,toolUseId,v" || rows[1]["reportSummary"] != "tt queue action (+3 args)" {
		t.Fatalf("would-report row = %v", rows[1])
	}

	// A read, a post, a tool other than Bash and the gate switched off leave nothing.
	count = len(toolLedgerRows(t, root, e.agent))
	toolHook(t, e, "PreToolUse", bashPre("u4", "tt work-items list; curl http://gatehub.test:8443/api/x; sqlite3 /tmp/x.db"))
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u5", "tool_name": "Write", "tool_input": map[string]any{"file_path": "/repo/run.sh", "command": command, "content": command}})
	setToolLedger(t, &toolGateOn, false)
	toolHook(t, e, "PreToolUse", bashPre("u6", toolGateAllRules))
	setToolLedger(t, &toolGateOn, true)
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != count {
		t.Fatalf("rows from calls that match nothing = %v", rows[count:])
	}
	toolHook(t, e, "PostToolUse", bashPre("u7", toolGateAllRules))
	toolHook(t, e, "PostToolUseFailure", bashPre("u8", toolGateAllRules))
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != count+2 || len(wouldReportRows(rows[count:])) != 0 {
		t.Fatalf("rows from a post and a failure = %v", rows[count:])
	}
}

// a5: the tag comes from the identity the hook already holds and, for a
// lead, the form of the command. Tagged rows are written, not dropped.
func TestToolGateTags(t *testing.T) {
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)   // the tags are the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	const create, plan, receipt = "tt work-items create --title x", "tt verification plan --item wi_1 --file p.json", "tt verification receipt --item wi_1 --check v1"
	const scope = "tt team queue scope --entry tqe_1 --owns a.go"
	// The item id in toolGateDoneSave belongs to no entry the hook knows of:
	// it keeps no state and may not ask the hub whose entry a command names.
	// So a lead running the done save for an item that is not its own is
	// still lead-allowed. The tag is a name-and-form heuristic.
	cases := []struct{ name, command, tag, agentName string }{
		{"db-handler-o3", create, "handler", "db-handler-o3"},
		{"db-handler-o3", toolGateAllRules, "handler,handler,handler", "db-handler-o3"},
		{"db-handler", plan, "handler", "db-handler"},
		{"lead-x", plan, "lead-allowed", "lead-x"},
		{"lead-x", receipt, "lead-allowed", "lead-x"},
		{"lead-x", toolGateDoneSave, "lead-allowed", "lead-x"},
		{"lead-x", create, "none", "lead-x"},
		{"lead-x", "tt work-items update wi_1 --status done", "none", "lead-x"},
		{"lead-x", "tt team queue accept --entry tqe_1", "none", "lead-x"},
		{"lead-x", plan + "; " + create, "none", "lead-x"},
		{"lead-x", toolGateAllRules, "none,none,none", "lead-x"},
		{"builder-x", create, "none", "builder-x"},
		{"builder-x", plan, "none", "builder-x"},
		// k7: a lead's scope of a queue entry and a verifier's receipt are allowed.
		{"verifier-x", receipt, "verifier-allowed", "verifier-x"},
		{"verifier-9ac4285c", "cd /w && " + receipt + " 2>&1 | tail -3", "verifier-allowed", "verifier-9ac4285c"},
		{"verifier-x", plan, "none", "verifier-x"},
		{"verifier-x", scope, "none", "verifier-x"},
		{"verifier-x", toolGateDoneSave, "none", "verifier-x"},
		{"verifier-x", receipt + "; " + plan, "none", "verifier-x"},
		{"verifier-x", create + "; " + receipt, "none", "verifier-x"},
		{"verifier-x", toolGateAllRules, "none,none,none", "verifier-x"},
		{"builder-x", receipt, "none", "builder-x"},
		{"reviewer-x", receipt, "none", "reviewer-x"},
		{"my-verifier-x", receipt, "none", "my-verifier-x"},
		{"db-handler-o3", receipt, "handler", "db-handler-o3"},
		{"lead-x", scope, "lead-allowed", "lead-x"},
		{"lead-x", receipt + "; " + plan + "; " + scope, "lead-allowed", "lead-x"},
		{"lead-x", scope + "; " + create, "none", "lead-x"},
		{"lead-x", "tt team queue wait set --entry tqe_1 --on tqe_2", "none", "lead-x"},
		{"builder-x", scope, "none", "builder-x"},
		{"my-lead-x", scope, "none", "my-lead-x"},
		// k5: the done save typed with single-dash flags.
		{"lead-x", toolGateDoneSaveOneDash, "lead-allowed", "lead-x"},
		{"lead-x", "tt work-items update wi_1 -status=done -worktree=/w -branch=b -commit=c", "lead-allowed", "lead-x"},
		{"lead-x", "tt work-items update wi_1 -status done -worktree /w", "none", "lead-x"},
		{"builder-x", toolGateDoneSaveOneDash, "none", "builder-x"},
		{"my-db-handler", create, "none", "my-db-handler"},
		{"", create, "none", ""},
		{"bad name; rm", create, "none", ""},
		{strings.Repeat("n", 200), create, "none", strings.Repeat("n", 128)},
	}
	for i, c := range cases {
		e, root := gateSandbox(t)
		e.agentName = c.name
		toolHook(t, e, "PreToolUse", bashPre(fmt.Sprintf("u%d", i), c.command))
		rows := wouldReportRows(toolLedgerRows(t, root, e.agent))
		var tags []string
		for _, row := range rows {
			tags = append(tags, fmt.Sprint(row["reportTag"]))
			if name, _ := row["agentName"].(string); name != c.agentName {
				t.Errorf("%q running %s: agentName = %v; want %q", c.name, c.command, row["agentName"], c.agentName)
			}
		}
		if strings.Join(tags, ",") != c.tag {
			t.Errorf("%q running %s: tags = %q; want %q", c.name, c.command, tags, c.tag)
		}
	}

	// The owner helper: the identity, and the name, come from its file.
	root, _ := helperLedgerSandbox(t)
	t.Setenv("TAILTERM_REDACT_SETTING_FILE", filepath.Join(t.TempDir(), "relay.json"))
	file := helperLedgerFile()
	writeHelperLedgerFile(t, file)
	t.Setenv("CLAUDE_CODE_SESSION_ID", file.Thread)
	e := helperLedgerEnv(t)
	t.Setenv("TAILTERM_HUB", toolGateTestHub)
	for i, command := range []string{toolGateAllRules, plan, toolGateDoneSave} {
		toolHook(t, e, "PreToolUse", map[string]any{"session_id": file.Thread, "tool_use_id": fmt.Sprintf("h%d", i), "tool_name": "Bash", "tool_input": map[string]any{"command": command}})
	}
	rows := wouldReportRows(toolLedgerRows(t, root, file.Agent))
	if len(rows) != 5 {
		t.Fatalf("helper rows = %v; want five", rows)
	}
	for _, row := range rows {
		if row["reportTag"] != "owner-helper" || row["agentName"] != file.Name || row["agent"] != file.Agent {
			t.Errorf("helper row = %v; want the owner-helper tag and the file's name", row)
		}
	}
}

// a6: nothing typed in the command reaches the ledger, no file it names is
// opened, and nothing the host's hub.json holds reaches a row.
func TestToolGateRedaction(t *testing.T) {
	e, root := gateSandbox(t)
	// What the rows hold is the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row.
	// A hook that did open a FIFO below still ends without its rows, at this
	// deadline instead of the real one.
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	// Each row's time is scanned with the rest, so it is one that holds no
	// secret: the real clock's fraction digits can hold 8443.
	setToolLedgerClock(t, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	// A FIFO with no writer blocks whoever opens it for reading. The hook
	// returning with its rows written shows neither was opened: the file a
	// command names with @, and hub.json where the configuration is read.
	// This test calls the handler directly, after that read would have run.
	config := filepath.Join(dir, "home", ".config", "tailterm")
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(dir, "home"))
	fifo := filepath.Join(dir, "headers-sentinel.txt")
	for _, path := range []string{fifo, filepath.Join(config, "hub.json")} {
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The two values the configuration read can take from hub.json. The gate
	// reads the hub address from TAILTERM_HUB alone, which is another one.
	e.token, e.hub, e.configHub = "tok-config-sentinel-0123456789abcdef", "http://confighub-sentinel.test:18999", "http://confighub-sentinel.test:18999"
	t.Setenv("TAILTERM_TOKEN", "")
	const bearer = "tt_bearer-sentinel-0123456789abcdefghij"
	commands := []string{
		`curl -sS -X POST -H "Authorization: Bearer ` + bearer + `" -H @` + fifo + ` -d '{"title":"body-sentinel"}' http://user-sentinel:pass-sentinel@gatehub.test:8443/api/tasks/tsk_sentinel/work-items`,
		`curl --data-binary @` + fifo + ` "$TAILTERM_HUB/api/path-sentinel?key=query-sentinel"`,
		`tt work-items update wi_sentinel0123 --status done --worktree /private/wt-sentinel --branch branch-sentinel --commit c0ffee-sentinel --hub http://flaghub-sentinel:18765 --token flagtoken-sentinel`,
		`sqlite3 -cmd '.read ` + fifo + `' /mnt/deepfreeze/tailterm-hub/db-sentinel.sqlite "select 'sql-sentinel'"`,
		`ssh root-sentinel@truenas "sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db 'update sql-sentinel'"`,
	}
	for i, command := range commands {
		toolHook(t, e, "PreToolUse", bashPre(fmt.Sprintf("u%d", i), command))
	}
	// A write to the configured address is no hub write: only TAILTERM_HUB counts.
	toolHook(t, e, "PreToolUse", bashPre("u9", `curl -X POST http://confighub-sentinel.test:18999/api/x`))
	rows := toolLedgerRows(t, root, e.agent)
	want := []string{
		"hub-direct-write/none/curl POST data hub (+9 args)",
		"hub-direct-write/none/curl data hub (+2 args)",
		"work-item-write/none/tt work-items update --status done --worktree --branch --commit (+8 args)",
		"hub-db-direct/none/sqlite3 hub-db (+3 args)",
		"hub-db-direct/none/sqlite3 hub-db (+1 args)",
	}
	if got := gateRowTexts(rows); len(rows) != len(want) || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rows = %q of %d; want %q", got, len(rows), want)
	}
	ledger, err := os.ReadFile(filepath.Join(root, e.agent, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	secrets := append([]string{"sentinel", bearer, e.token, "Bearer", "Authorization", "gatehub", "8443", "18999", "18765\"", "truenas", "/mnt/", "deepfreeze", fifo, dir, "@", "://"}, commands...)
	hits, err := ledgerSecretHits(ledger, secrets)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range hits {
		t.Errorf("the ledger holds %q:\n%s", secret, ledger)
	}
}

// ledgerSecretHits returns the secrets that the ledger holds anywhere but in
// a row's argsDigest. That digest is a SHA-256 over the command, which names
// a per-run temporary path, so its hex can hold a short secret such as 8443
// by chance. It must be exactly that, lowercase hex of SHA-256 length, so
// nothing else can pass unscanned in its place.
func ledgerSecretHits(ledger []byte, secrets []string) ([]string, error) {
	var scanned strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(ledger), "\n"), "\n") {
		var row struct {
			ArgsDigest *string `json:"argsDigest"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("ledger line %q: %v", line, err)
		}
		if row.ArgsDigest == nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(*row.ArgsDigest) {
			return nil, fmt.Errorf("ledger line %q: argsDigest is not lowercase SHA-256 hex", line)
		}
		field := `"argsDigest":"` + *row.ArgsDigest + `"`
		if strings.Count(line, field) != 1 {
			return nil, fmt.Errorf("ledger line %q: holds %s %d times; want once", line, field, strings.Count(line, field))
		}
		scanned.WriteString(strings.Replace(line, field, `"argsDigest":""`, 1) + "\n")
	}
	var hits []string
	for _, secret := range secrets {
		if strings.Contains(scanned.String(), secret) {
			hits = append(hits, secret)
		}
	}
	return hits, nil
}

// The redaction check skips a digest that holds a secret by chance, and still
// finds that secret in any other field or in a value that is no digest.
func TestLedgerSecretHitsSkipsOnlyDigest(t *testing.T) {
	digest := "1c7f3cc3" + strings.Repeat("0", 44) + "9e808443f4ab"
	row := func(fields string) string {
		return `{"v":1,"time":"2026-10-06T12:00:00Z","task":"tsk_ledger","agent":"agt_ledger","run":"run_ledger","session":"s1","tool":"Bash","toolUseId":"u0","argsDigest":"` + digest + `","outcome":"would_report"` + fields + "}\n"
	}
	secrets := []string{"8443", "@"}
	for _, c := range []struct {
		name, ledger string
		want         []string
	}{
		{"digest", row(""), nil},
		{"summary", row(`,"reportSummary":"curl gatehub 8443"`), []string{"8443"}},
		{"tool use ID", row("") + strings.Replace(row(""), `"u0"`, `"u8443"`, 1), []string{"8443"}},
		{"time", strings.Replace(row(""), "12:00:00Z", "12:00:00.18443Z", 1), []string{"8443"}},
		{"other secret", row(`,"reportPath":"@"`), []string{"@"}},
	} {
		hits, err := ledgerSecretHits([]byte(c.ledger), secrets)
		if err != nil || strings.Join(hits, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: hits = %q, %v; want %q", c.name, hits, err, c.want)
		}
	}
	for _, bad := range []string{"8443", strings.ToUpper(digest), digest + "8443"} {
		ledger := strings.Replace(row(""), digest, bad, 1)
		if hits, err := ledgerSecretHits([]byte(ledger), secrets); err == nil {
			t.Errorf("argsDigest %q: hits = %q with no error; want it refused unscanned", bad, hits)
		}
	}
	if _, err := ledgerSecretHits([]byte(strings.Replace(row(""), `,"argsDigest":"`+digest+`"`, "", 1)), secrets); err == nil {
		t.Error("a row with no argsDigest: no error; want it refused")
	}
}

// a7: whatever goes wrong, no gate row is written, the hook returns nil and
// prints nothing, and the call's ordinary row is written when it ends.
func TestToolGateFailsOpen(t *testing.T) {
	const command = "pkill node; " + toolGateAllRules
	// What each failure leaves in the ledger is the subject here, not timing:
	// a hook that reaches its deadline on a loaded host writes no row.
	// The deadline passed case sets its own deadlines.
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	finish := func(t *testing.T, e env, root string, deny int) {
		t.Helper()
		rows := toolLedgerRows(t, root, e.agent)
		if len(wouldReportRows(rows)) != 0 || len(wouldDenyRows(rows)) != deny || len(rows) != deny {
			t.Fatalf("rows = %v; want %d would-deny rows and no others", rows, deny)
		}
		if files := toolPendingFiles(t, root, e.agent); len(files) != 1 {
			t.Fatalf("pending files = %d; want 1", len(files))
		}
		toolHook(t, e, "PostToolUse", bashPre("u1", command))
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != deny+1 || rows[deny]["outcome"] != "ok" || rows[deny]["durationSource"] != "measured" {
			t.Fatalf("rows after the post = %v", rows)
		}
	}
	t.Run("gate panics", func(t *testing.T) {
		e, root := gateSandbox(t)
		setToolLedger(t, &toolGateMatch, func(*toolDenyScan, string, []string) { panic("gate on fire") })
		toolHook(t, e, "PreToolUse", bashPre("u1", command))
		finish(t, e, root, 1) // the would-deny row is written as before
	})
	t.Run("gate panics after a match", func(t *testing.T) {
		e, root := gateSandbox(t)
		setToolLedger(t, &toolGateMatch, func(c *toolDenyScan, name string, args []string) {
			c.gateCommand(name, args)
			if name == "sqlite3" {
				panic("gate on fire")
			}
		})
		toolHook(t, e, "PreToolUse", bashPre("u1", command))
		finish(t, e, root, 1)
	})
	t.Run("check panics", func(t *testing.T) {
		e, root := gateSandbox(t)
		setToolLedger(t, &toolDenyCheck, func(toolLedger, toolLedgerRow, string, json.RawMessage) { panic("classifier on fire") })
		toolHook(t, e, "PreToolUse", bashPre("u1", command))
		finish(t, e, root, 0)
	})
	t.Run("write panics inside the check", func(t *testing.T) {
		_, root := gateSandbox(t)
		l := toolLedger{dir: filepath.Join(root, "agt_ledger"), agent: "agt_ledger", started: time.Now(), deadline: time.Minute, denyMax: toolDenyMaxCommand, redactBudget: time.Minute, gate: true, name: "builder-x"}
		l.wouldDeny(toolLedgerRow{}, "Bash", json.RawMessage(`{"command":"`+toolGateAllRules+`"}`)) // l.now is nil
		requireEmptyRoot(t, root)
	})
	t.Run("command over the cap", func(t *testing.T) {
		e, root := gateSandbox(t)
		setToolLedger(t, &toolDenyMaxCommand, len(command)+8)
		toolHook(t, e, "PreToolUse", bashPre("u1", command+"; echo "+strings.Repeat("x", 8)))
		finish(t, e, root, 0)
	})
	t.Run("command at the cap", func(t *testing.T) {
		e, root := gateSandbox(t)
		setToolLedger(t, &toolDenyMaxCommand, len(command))
		toolHook(t, e, "PreToolUse", bashPre("u1", command))
		if rows := toolLedgerRows(t, root, e.agent); len(wouldReportRows(rows)) != 3 || len(wouldDenyRows(rows)) != 1 {
			t.Fatalf("rows = %v; want the four rows", rows)
		}
	})
	t.Run("substitutions nested nine deep", func(t *testing.T) {
		e, root := gateSandbox(t)
		deep := strings.Repeat("$(", toolDenyMaxNest+1) + "tt work-items create x" + strings.Repeat(")", toolDenyMaxNest+1)
		toolHook(t, e, "PreToolUse", bashPre("u1", "echo "+deep))
		finish(t, e, root, 0)
		eight := strings.Repeat("$(", toolDenyMaxNest) + "tt work-items create x" + strings.Repeat(")", toolDenyMaxNest)
		toolHook(t, e, "PreToolUse", bashPre("u2", "echo "+eight))
		if rows := toolLedgerRows(t, root, e.agent); len(wouldReportRows(rows)) != 1 {
			t.Fatalf("rows = %v; want the row of a command nested to the bound", rows)
		}
	})
	t.Run("no command", func(t *testing.T) {
		e, root := gateSandbox(t)
		toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": map[string]any{"command": 7, "script": command}})
		toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u2", "tool_name": "Bash", "tool_input": command})
		toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u3", "tool_name": "Bash"})
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != 0 {
			t.Fatalf("rows = %v; want none", rows)
		}
	})
	t.Run("deadline passed", func(t *testing.T) {
		// On a loaded host the hook can reach its 40 ms deadline before the
		// check is called, and then it never is: that attempt shows nothing
		// about a late check, so it is run again in a new sandbox, at most
		// three times. The last attempt is judged as it stands.
		var e env
		var root string
		for attempt := 1; ; attempt++ {
			e, root = gateSandbox(t)
			setToolLedger(t, &toolLedgerDeadline, 40*time.Millisecond)
			called, finished := make(chan struct{}), make(chan struct{})
			setToolLedger(t, &toolDenyCheck, func(l toolLedger, row toolLedgerRow, tool string, input json.RawMessage) {
				close(called)
				defer close(finished)
				time.Sleep(time.Until(l.started.Add(l.deadline + 20*time.Millisecond)))
				l.wouldDeny(row, tool, input)
			})
			toolHook(t, e, "PreToolUse", bashPre("u1", command))
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				select {
				case <-called:
				default:
					if attempt < 3 {
						t.Logf("attempt %d: the hook reached its %v deadline before the check was called; running it again in a new sandbox", attempt, toolLedgerDeadline)
						continue
					}
				}
				t.Fatal("the late check never finished")
			}
			break
		}
		setToolLedger(t, &toolLedgerDeadline, 30*time.Second) // the post hook's row is not about timing: a hook that reaches its deadline on a loaded host writes no row
		finish(t, e, root, 0)
	})
	t.Run("unreadable input", func(t *testing.T) {
		e, root := gateSandbox(t)
		pre := toolPayload("PreToolUse", bashPre("u1", command))
		for _, payload := range []string{pre[:len(pre)-1], "", `{"hook_event_name":"Nope","tool_name":"Bash","tool_input":{"command":"tt work-items create"}}`} {
			if r := runToolHook(t, e, "tool", payload, nil); r.err != nil || r.out != "" {
				t.Fatalf("hook = %v, output %q", r.err, r.out)
			}
		}
		setToolLedger(t, &toolLedgerMaxInput, 256)
		big := bashPre("u2", command)
		big["description"] = strings.Repeat("d", 512)
		toolHook(t, e, "PreToolUse", big)
		rows := toolLedgerRows(t, root, e.agent)
		if len(rows) != 4 {
			t.Fatalf("rows = %v; want four", rows)
		}
		for _, row := range rows {
			if row["outcome"] != "unreadable" || row["reportRule"] != nil || row["agentName"] != nil {
				t.Fatalf("row = %v; want unreadable only", row)
			}
		}
	})
}

// a7: matching and non-matching calls alike return nil and print nothing,
// on all three events.
func TestToolGatePrintsNothing(t *testing.T) {
	e, root := gateSandbox(t)
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)   // silence and the row counts are the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	for i, command := range []string{"tt work-items list", "tt work-items create --title x", toolGateAllRules, "pkill node; " + toolGateAllRules} {
		for _, event := range []string{"PreToolUse", "PostToolUse", "PreToolUse", "PostToolUseFailure"} {
			if r := runToolHook(t, e, "tool", toolPayload(event, bashPre(fmt.Sprintf("u%d", i), command)), nil); r.err != nil || r.out != "" {
				t.Fatalf("%s %q = %v, output %q; want nil and no output", event, command, r.err, r.out)
			}
		}
	}
	rows := toolLedgerRows(t, root, e.agent)
	if got := len(wouldReportRows(rows)); got != 2*(0+1+3+3) {
		t.Fatalf("would-report rows = %d; want 14", got)
	}
	if got := len(wouldDenyRows(rows)); got != 2 {
		t.Fatalf("would-deny rows = %d; want 2", got)
	}
}

func toolGateCapCommand(head string) string {
	filler := `echo "a quoted tt work-items create; curl -X POST http://h:18765/x" 'sqlite3 /mnt/deepfreeze/tailterm-hub/hub.db' $(date) | grep -c x && tt work-items list && curl -sS http://gatehub.test:8443/api/x; `
	return head + strings.Repeat(filler, (toolDenyMaxCommand-len(head))/len(filler))
}

// a9: the added cost of the gate per PreToolUse, measured against the same
// hook with the gate switched off and the would-deny check on in both.
// Every run with the gate that returns before the hook's deadline has its
// rows already written. The limits, and the runs put down to a stalled host
// and run again, are those of TestToolDenyCost.
func TestToolGateCost(t *testing.T) {
	const runs = 100
	limit := 5 * time.Millisecond
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "-race" && setting.Value == "true" {
				limit = 25 * time.Millisecond
				t.Logf("race detector on: the added-cost limit is %v", limit)
			}
		}
	}
	e, root := gateSandbox(t)
	cases := []struct {
		name, command string
		rows, deny    int
	}{
		{"no match, short", "go test ./cmd/tt -run TestToolGate -count=1", 0, 0},
		{"tt command that is no write", `tt send --kind result --to lead-x --subject "Tests pass" --outcome done`, 0, 0},
		{"one rule", "tt work-items create --title x", 1, 0},
		{"three rules", toolGateAllRules, 3, 0},
		{"three rules and four deny rules", toolGateAllRules + "; " + toolDenyAllRules, 3, 4},
		{"no match at the 64 KiB cap", toolGateCapCommand(""), 0, 0},
		{"three rules at the 64 KiB cap", toolGateCapCommand(toolGateAllRules + "; "), 3, 0},
	}
	for n, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var with, without []time.Duration
			late := 0
			var worst time.Duration
			// As in TestToolDenyCost: a run with the gate that reached the
			// deadline may fail open with no rows, so it is put down to a
			// stalled host and run again as a new agent after a wait, and the
			// stalled attempts of a case are capped. A baseline run that
			// reached it is set aside as before.
			const attempts, maxStalled = 3, 10
			for i := 0; i < 2*runs; i++ {
				// Alternate, and swap which goes first each pair.
				gate := (i+i/2)%2 == 0
				for attempt := 1; ; attempt++ {
					toolGateOn = gate
					e.agent = fmt.Sprintf("agt_gate_%d_%d_%d", n, i, attempt)
					r := runToolHook(t, e, "tool", toolPayload("PreToolUse", bashPre("u1", c.command)), nil)
					toolGateOn = true
					if r.err != nil || r.out != "" {
						t.Fatalf("hook = %v, output %q", r.err, r.out)
					}
					all := toolLedgerRows(t, root, e.agent)
					rows, deny := len(wouldReportRows(all)), len(wouldDenyRows(all))
					switch {
					case r.elapsed >= toolLedgerDeadline:
						t.Logf("run %d attempt %d (gate %v) reached the %v deadline in %v with %d of %d rows", i, attempt, gate, toolLedgerDeadline, r.elapsed, rows+deny, c.rows+c.deny)
						if !gate {
							break
						}
						late++
						if attempt == attempts {
							t.Fatalf("run %d reached the %v deadline %d times in a row; a stalled host does not explain that", i, toolLedgerDeadline, attempts)
						}
						if late > maxStalled {
							t.Fatalf("%d attempts in %d runs reached the hook deadline; at most %d are put down to a stalled host", late, runs, maxStalled)
						}
						time.Sleep(time.Duration(attempt) * time.Second)
						continue
					case !gate:
						if rows != 0 || deny != c.deny {
							t.Fatalf("baseline run wrote %d would-report and %d would-deny rows; want 0 and %d", rows, deny, c.deny)
						}
						without = append(without, r.elapsed)
					default:
						if rows != c.rows || deny != c.deny {
							t.Fatalf("run %d returned in %v with %d would-report and %d would-deny rows; want %d and %d", i, r.elapsed, rows, deny, c.rows, c.deny)
						}
						with = append(with, r.elapsed)
						worst = max(worst, r.elapsed)
					}
					break
				}
			}
			if late > 0 {
				t.Logf("%d attempt(s) with the gate were put down to a stalled host and run again; the cap is %d", late, maxStalled)
			}
			base, gated := toolDenyMedian(without), toolDenyMedian(with)
			t.Logf("%d bytes: baseline median %v, with the gate %v, added %v, worst run with the gate %v", len(c.command), base, gated, gated-base, worst)
			if gated-base >= limit {
				t.Fatalf("the gate adds %v to the median hook; want under %v", gated-base, limit)
			}
		})
	}
}

// m5: a tt write subcommand alone, or followed only by help or version
// words, writes no work-item-write row. Any other word after it does.
func TestToolGateHelpCalls(t *testing.T) {
	for _, command := range []string{
		"tt work-items update", "tt work-items update --help", "tt work-items update -h", "tt work-items update -help",
		"tt work-items update -version", "tt work-items update --version", "tt work-items create -h --help",
		"tt work-items create", "tt work-items scope confirm --help", "tt queue action", "tt queue action -h",
		"tt team queue wait set", "tt team queue wait set --help", "tt team queue scope --help", "tt team queue accept",
		"tt verification plan", "tt verification plan --help", "tt verification receipt -version",
		"sudo /Users/x/.local/bin/tt work-items update --help", "bash -c 'tt work-items update --help'",
		"ssh air 'tt work-items create -h'", "tt work-items update --help 2>&1 | head -20", "tt work-items update --help; tt work-items list",
	} {
		if _, gates := toolGateClassify(command, toolGateTestHub, true); len(gates) != 0 {
			t.Errorf("%s: matches %v; want none", command, gates)
		}
	}
	for command, want := range map[string]struct{ kind, summary string }{
		toolGateDoneSave:                                       {toolGateLeadForm, "tt work-items update --status done --worktree --branch --commit (+4 args)"},
		toolGateDoneSave + " --help":                           {toolGateLeadForm, "tt work-items update --status done --worktree --branch --commit (+5 args)"},
		"tt work-items update wi_1":                            {"", "tt work-items update (+1 args)"},
		"tt work-items update wi_1 --status open":              {"", "tt work-items update (+3 args)"},
		"tt work-items update wi_1 --help":                     {"", "tt work-items update (+2 args)"},
		"tt work-items update --help wi_1":                     {"", "tt work-items update (+2 args)"},
		"tt work-items update help":                            {"", "tt work-items update (+1 args)"},
		"tt work-items update --helpful":                       {"", "tt work-items update (+1 args)"},
		"tt work-items update -H":                              {"", "tt work-items update (+1 args)"},
		"tt work-items update --help=true":                     {"", "tt work-items update (+1 args)"},
		"tt work-items update ''":                              {"", "tt work-items update (+1 args)"},
		"tt team queue wait set --help --entry tqe_1":          {"", "tt team queue wait set (+3 args)"},
		"tt verification receipt --help --item wi_1":           {toolGateReceiptForm, "tt verification receipt (+3 args)"},
		"tt work-items update --help; tt work-items note wi_1": {"", "tt work-items note (+1 args)"},
		"tt work-items note wi_1; tt work-items update --help": {"", "tt work-items note (+1 args)"},
	} {
		_, gates := toolGateClassify(command, toolGateTestHub, true)
		if len(gates) != 1 || gates[0].rule != toolGateWorkItem || gates[0].kind != want.kind || strings.Join(gates[0].parts, " ") != want.summary {
			t.Errorf("%s: matches %v; want one %s of kind %q, summary %q", command, gates, toolGateWorkItem, want.kind, want.summary)
		}
	}
	// Through the hook: the help call leaves no report row, the write its one.
	e, root := gateSandbox(t)
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second)   // the one row is the subject here, not timing: a hook that reaches its deadline on a loaded host writes no row
	setToolLedger(t, &toolRedactScanBudget, 30*time.Second) // nor is the scan budget: a hook that passes it writes its rows with no summary
	toolHook(t, e, "PreToolUse", bashPre("u1", "tt work-items update --help"))
	toolHook(t, e, "PreToolUse", bashPre("u2", "tt work-items update"))
	if rows := wouldReportRows(toolLedgerRows(t, root, e.agent)); len(rows) != 0 {
		t.Fatalf("rows = %v; want none for a help call", rows)
	}
	toolHook(t, e, "PreToolUse", bashPre("u3", "tt work-items update wi_1 --status open"))
	if rows := wouldReportRows(toolLedgerRows(t, root, e.agent)); len(rows) != 1 || rows[0]["reportSummary"] != "tt work-items update (+3 args)" {
		t.Fatalf("rows = %v; want the one row of the write", rows)
	}
}

// a10: the command docs/claude-wake.md gives counts handler-gate rows by
// rule, tag and agent name, across agents and a rotated file, past a torn
// last line, and counts no other kind of row.
func TestToolGateDocCommand(t *testing.T) {
	const jq = "/usr/bin/jq"
	if _, err := os.Stat(jq); err != nil {
		t.Skipf("%s is not installed", jq)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "claude-wake.md"))
	if err != nil {
		t.Fatal(err)
	}
	command := ""
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.Contains(line, `select(.reportTag != null)`) {
			if command != "" {
				t.Fatal("docs/claude-wake.md gives the handler-gate summary command more than once")
			}
			command = line
		}
	}
	if !strings.HasPrefix(command, "awk 1 ~/.local/state/tailterm/tool-ledger/") || !strings.Contains(command, jq) || strings.Contains(command, ".outcome ==") {
		t.Fatalf("docs/claude-wake.md does not give the handler-gate summary command: %q", command)
	}
	home := t.TempDir()
	write := func(agent, name string, lines ...string) {
		dir := filepath.Join(home, ".local", "state", "tailterm", "tool-ledger", agent)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gate := func(agent, name, rule, tag string) string {
		b, _ := json.Marshal(toolLedgerRow{V: 1, Agent: agent, Tool: "Bash", Outcome: "would_report", ReportRule: rule, ReportTag: tag, AgentName: name})
		return string(b)
	}
	other := func(row toolLedgerRow) string {
		b, _ := json.Marshal(row)
		return string(b)
	}
	scope := other(toolLedgerRow{V: 1, Agent: "agt_a", Tool: "Edit", Outcome: "would_report", ReportRule: "outside-owned-paths", ReportPath: "docs/x.md", AssignSeq: 7})
	deny := other(toolLedgerRow{V: 1, Agent: "agt_a", Tool: "Bash", Outcome: "would_deny", DenyRule: "host-pkill"})
	write("agt_a", "ledger.jsonl.1", gate("agt_a", "builder-x", "work-item-write", "none"), other(toolLedgerRow{V: 1, Agent: "agt_a", Outcome: "ok"}), "")
	write("agt_a", "ledger.jsonl", gate("agt_a", "builder-x", "work-item-write", "none"), gate("agt_a", "builder-x", "hub-direct-write", "none"), scope, deny, `{"v":1,"agent":"agt_a","outcome":"would_rep`)
	write("agt_b", "ledger.jsonl", gate("agt_b", "db-handler-o3", "work-item-write", "handler"), gate("agt_b", "db-handler-o3", "work-item-write", "handler"), gate("agt_b", "db-handler-o3", "hub-db-direct", "handler"), "")
	write("agt_c", "ledger.jsonl", gate("agt_c", "lead-x", "work-item-write", "lead-allowed"), gate("agt_c", "", "work-item-write", "none"), "")
	write("agt_d", "ledger.jsonl", gate("agt_d", "owner-helper", "hub-direct-write", "owner-helper"), scope, "")
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", command, err, out)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	want := []string{
		"1 hub-db-direct handler db-handler-o3", "1 hub-direct-write none builder-x", "1 hub-direct-write owner-helper owner-helper",
		"2 work-item-write handler db-handler-o3", "1 work-item-write lead-allowed lead-x", "1 work-item-write none -", "2 work-item-write none builder-x",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary = %q; want %q", got, want)
	}
}
