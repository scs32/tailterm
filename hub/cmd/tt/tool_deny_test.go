package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
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
		{"old-site-deploy", `./scripts/deploy-static.mjs`},

		{"truenas-tailscale", `ssh truenas tailscale status`},
		{"truenas-tailscale", `ssh truenas_admin@truenas-tailarr 'sudo tailscale up --ssh'`},
		{"truenas-tailscale", `ssh -o BatchMode=yes truenas "midclt call app.query; systemctl restart tailscaled"`},
		{"truenas-tailscale", `ssh truenas_admin@100.116.238.37 tailscale status`},
		{"truenas-tailscale", `ssh -p 22 TrueNAS.local Tailscale down`},

		{"host-pkill,deployer-signal,old-site-deploy,truenas-tailscale", `pkill -f tt-deployer; npm run deploy:static && ssh truenas tailscale status`},
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
		// deployer-signal
		`tmux has-session -t tt-deployer-compact`,
		`tmux capture-pane -p -t tt-deployer-compact`,
		`tmux ls | grep tt-deployer`,
		`tmux kill-session -t tt-builder-1`,
		`tmux -L test kill-server`,
		`echo tt-deployer-compact; kill 12345`,
		// old-site-deploy
		`npm run deploy:tailos`,
		`npm run build`,
		`cat scripts/deploy-static.mjs`,
		`grep -n deploy:static package.json`,
		`node --check scripts/deploy-static.mjs`,
		`node scripts/deploy-static.mjs --check`,
		`node -e "console.log(1)" scripts/deploy-static.mjs`,
		`git log -- scripts/deploy-static.mjs`,
		`tt send --subject "do not run npm run deploy:static"`,
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
		e, root := toolLedgerSandbox(t)
		setToolLedger(t, &toolLedgerDeadline, 40*time.Millisecond)
		finished := make(chan struct{})
		setToolLedger(t, &toolDenyCheck, func(l toolLedger, row toolLedgerRow, tool string, input json.RawMessage) {
			defer close(finished)
			time.Sleep(time.Until(l.started.Add(l.deadline + 20*time.Millisecond)))
			l.wouldDeny(row, tool, input)
		})
		toolHook(t, e, "PreToolUse", bashPre("u1", "pkill node"))
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("the late check never finished")
		}
		setToolLedger(t, &toolLedgerDeadline, 150*time.Millisecond)
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

const toolDenyAllRules = "pkill -f tt-deployer; npm run deploy:static; ssh truenas tailscale status"

// a6: the added cost of the check per PreToolUse, measured against the same
// hook with the check replaced by nothing. Every run with the check returns
// before the hook's deadline with its would-deny rows already written.
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
	}
	for n, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var with, without []time.Duration
			late := 0
			var worst time.Duration
			for i := 0; i < 2*runs; i++ {
				// Alternate, and swap which goes first each pair.
				check := (i+i/2)%2 == 0
				toolDenyCheck = nil
				if check {
					toolDenyCheck = real
				}
				e.agent = fmt.Sprintf("agt_cost_%d_%d", n, i)
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
					t.Logf("run %d reached the %v deadline in %v with %d of %d rows", i, toolLedgerDeadline, r.elapsed, rows, c.rows)
				default:
					if rows != c.rows {
						t.Fatalf("run %d returned in %v with %d would-deny rows; want %d", i, r.elapsed, rows, c.rows)
					}
					with = append(with, r.elapsed)
					worst = max(worst, r.elapsed)
				}
			}
			if late > 2 {
				t.Fatalf("%d of %d runs reached the hook deadline; at most 2 are put down to a stalled host", late, runs)
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
