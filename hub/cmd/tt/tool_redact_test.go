package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	mathrand "math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The sandbox probe of secret redaction (feature wi_79001641d97cd64c). A
// disposable `claude -p`
// session in a temporary project runs this test binary as its PostToolUse and
// PostToolUseFailure hook beside a freshly built tt. The built tt answers in
// the redact and report checks; the helper only logs there, and misbehaves
// (wrong shape, hanging, half an answer) in the others with tt switched off.
//
// Every value the probe looks for is generated at random for the run. None is
// written in this file, and no log line, failure message or finding holds one.

const (
	redactProbeHelperEnv  = "TT_REDACT_PROBE_HELPER"  // "hook" or "resolve": this process is a helper
	redactProbeSettingEnv = "TT_REDACT_PROBE_SETTING" // the sandbox file naming the hook's mode
	redactProbeLogEnv     = "TT_REDACT_PROBE_LOG"     // the sandbox file the hook appends rows to
	redactProbeHolder     = "[tt-redacted:github-token]"
)

// The environment as the process received it. TestMain removes every
// TAILTERM_ name before a test runs, and the resolve helper has to answer for
// the environment the disposable session was given.
var redactProbeStartEnv = os.Environ()

// The names the design's section 7 lists. The disposable session gets none of
// them from this process.
var redactProbeListedEnv = []string{
	"TAILTERM_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY",
	"GITHUB_TOKEN", "GH_TOKEN", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_KEY", "DISCORD_TOKEN", "DISCORD_BOT_TOKEN",
	"TS_AUTHKEY", "TS_API_KEY",
}

var redactProbeShape = regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)

func redactProbeRandom(t *testing.T, n int) string {
	t.Helper()
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = letters[int(raw[i])%len(letters)]
	}
	return string(raw)
}

// redactProbeReplace replaces the probe's shape inside every string value and
// keeps every key, type and number as it was.
func redactProbeReplace(v any, count *int) any {
	switch x := v.(type) {
	case string:
		*count += len(redactProbeShape.FindAllStringIndex(x, -1))
		return redactProbeShape.ReplaceAllString(x, redactProbeHolder)
	case []any:
		for i := range x {
			x[i] = redactProbeReplace(x[i], count)
		}
	case map[string]any:
		for key := range x {
			x[key] = redactProbeReplace(x[key], count)
		}
	}
	return v
}

// TestRedactProbeHelper is not a test. It is this binary acting as the
// disposable session's hook, or answering how tt resolves its paths and token
// from the session's environment. It exits before the test runner prints.
func TestRedactProbeHelper(t *testing.T) {
	switch os.Getenv(redactProbeHelperEnv) {
	case "hook":
		redactProbeHook()
		os.Exit(0)
	case "resolve":
		for _, kv := range redactProbeStartEnv {
			if name, value, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "TAILTERM_") {
				os.Setenv(name, value)
			}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
			"ledger": toolLedgerRoot(), "relay": relayDir(), "setting": toolRedactSettingPath(),
			"tokenDigest": fmt.Sprintf("%x", sha256.Sum256([]byte(readEnv().token))),
		})
		os.Exit(0)
	}
}

func redactProbeHook() {
	mode, _ := os.ReadFile(os.Getenv(redactProbeSettingEnv))
	dec := json.NewDecoder(os.Stdin)
	dec.UseNumber()
	var in map[string]any
	if dec.Decode(&in) != nil {
		return
	}
	count := 0
	updated := redactProbeReplace(in["tool_response"], &count)
	if text, ok := in["error"].(string); ok {
		count += len(redactProbeShape.FindAllStringIndex(text, -1))
	}
	event, _ := in["hook_event_name"].(string)
	var keys []string
	if object, ok := in["tool_response"].(map[string]any); ok {
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
	}
	row, _ := json.Marshal(map[string]any{"event": event, "tool": in["tool_name"], "mode": string(mode), "count": count, "responseKeys": keys, "transcript": in["transcript_path"]})
	if f, err := os.OpenFile(os.Getenv(redactProbeLogEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil {
		_, _ = f.Write(append(row, '\n'))
		_ = f.Close()
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	answer := func(output any) {
		_ = enc.Encode(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "updatedToolOutput": output}})
	}
	// In the observe mode the helper only logs: the freshly built tt is the
	// hook that answers.
	switch string(mode) {
	case "shape":
		answer(redactProbeHolder) // a string where the tool returns an object
		_, _ = os.Stdout.Write(out.Bytes())
	case "half":
		answer(updated)
		_, _ = os.Stdout.Write(out.Bytes()[:out.Len()/2])
	case "hang":
		time.Sleep(60 * time.Second)
	}
}

type redactProbeRun struct {
	session    string
	toolResult string        // every tool result the session sent to the model, joined
	reply      string        // the model's final reply
	toolWait   time.Duration // from the tool call appearing to its result appearing
	isError    bool          // the tool result was marked as an error
}

type redactProbe struct {
	t           *testing.T
	root        string // the sandbox: everything the probe writes, apart from Claude Code's own files
	project     string
	env         []string
	claude      string
	setting     string
	log         string
	started     time.Time
	values      []string // every generated value, for the scans
	transcripts map[string]bool
}

// TestSecretRedactionSandboxProbe: run with TT_LIVE_CLAUDE=1. It starts eight
// disposable Claude sessions with an environment built from nothing and
// reports, per check, what the model received and which local files hold the
// generated value. The transcript and local-copy findings are records for the
// owner's decision D5, not pass or fail.
func TestSecretRedactionSandboxProbe(t *testing.T) {
	if os.Getenv("TT_LIVE_CLAUDE") != "1" {
		t.Skip("requires an explicit live Claude check")
	}
	p := newRedactProbe(t)

	// Check 1: a rewrite on a successful call.
	value := p.newValue()
	run := p.session("redact", "cat value.txt")
	if strings.Contains(run.toolResult, value) || strings.Contains(run.reply, value) {
		t.Errorf("check 1: the generated value reached the model after a rewrite (tool result %v, reply %v)", strings.Contains(run.toolResult, value), strings.Contains(run.reply, value))
	}
	t.Logf("check 1 (redact, successful call): value in tool result=%v, in reply=%v; placeholder in tool result=%v, in reply=%v", strings.Contains(run.toolResult, value), strings.Contains(run.reply, value), strings.Contains(run.toolResult, redactProbeHolder), strings.Contains(run.reply, redactProbeHolder))
	p.findings("check 1", run, value)
	p.ledgerRow("check 1", "Bash", "redact")

	// Check 2: report mode changes nothing and counts one match.
	value = p.newValue()
	before := len(p.rows())
	run = p.session("report", "cat value.txt")
	counted := -1
	for _, row := range p.rows()[before:] {
		if row["event"] == "PostToolUse" && row["tool"] == "Bash" {
			counted = int(row["count"].(float64))
		}
	}
	if !strings.Contains(run.toolResult, value) || counted != 1 {
		t.Errorf("check 2: report mode: value in tool result=%v, hook counted %d", strings.Contains(run.toolResult, value), counted)
	}
	t.Logf("check 2 (report): output unchanged=%v, hook counted=%d", strings.Contains(run.toolResult, value), counted)
	p.findings("check 2", run, value)
	p.ledgerRow("check 2", "Bash", "report")

	// Check 3: a rewrite of the wrong shape is not applied.
	value = p.newValue()
	run = p.session("shape", "cat value.txt")
	if !strings.Contains(run.toolResult, value) {
		t.Errorf("check 3: a wrong-shape rewrite did not leave the original output (placeholder in tool result=%v)", strings.Contains(run.toolResult, redactProbeHolder))
	}
	t.Logf("check 3 (wrong shape): original output used=%v, placeholder in tool result=%v", strings.Contains(run.toolResult, value), strings.Contains(run.toolResult, redactProbeHolder))

	// Check 4: a hook that hangs is abandoned at the entry's timeout.
	value = p.newValue()
	run = p.session("hang", "cat value.txt")
	if !strings.Contains(run.toolResult, value) || run.toolWait > 10*time.Second {
		t.Errorf("check 4: hanging hook: original output used=%v, tool call to result %s", strings.Contains(run.toolResult, value), run.toolWait.Round(100*time.Millisecond))
	}
	t.Logf("check 4 (hanging hook, 5 s entry timeout): original output used=%v, tool call to result %s", strings.Contains(run.toolResult, value), run.toolWait.Round(100*time.Millisecond))

	// Check 5: a failed call cannot be rewritten. A record of the known gap.
	value = p.newValue()
	before = len(p.rows())
	run = p.session("redact", "cat value.txt; exit 3")
	events := map[string]bool{}
	for _, row := range p.rows()[before:] {
		events[fmt.Sprint(row["event"])] = true
	}
	if !run.isError || !events["PostToolUseFailure"] {
		t.Errorf("check 5: the command did not fail as intended (error result=%v, hook events %v)", run.isError, events)
	}
	t.Logf("check 5 (redact, failed call): value reached the model=%v, placeholder in tool result=%v, hook events=%v", strings.Contains(run.toolResult, value), strings.Contains(run.toolResult, redactProbeHolder), events)
	p.findings("check 5", run, value)

	// Check 6: half of a JSON answer is not applied.
	value = p.newValue()
	run = p.session("half", "cat value.txt")
	if !strings.Contains(run.toolResult, value) {
		t.Errorf("check 6: a cut-off answer did not leave the original output (placeholder in tool result=%v)", strings.Contains(run.toolResult, redactProbeHolder))
	}
	t.Logf("check 6 (cut-off JSON): original output used=%v, placeholder in tool result=%v", strings.Contains(run.toolResult, value), strings.Contains(run.toolResult, redactProbeHolder))

	// Checks 7 and 8: the same two modes for a file read through the Read tool.
	const readPrompt = "Read the file value.txt in the current directory with the Read tool, once, and use no other tool. Then reply with the file's content copied exactly, character for character, and nothing else."
	value = p.newValue()
	before = len(p.rows())
	run = p.turn("redact", "Read", readPrompt)
	if strings.Contains(run.toolResult, value) || strings.Contains(run.reply, value) {
		t.Errorf("check 7: the generated value reached the model after a rewrite of Read output (tool result %v, reply %v)", strings.Contains(run.toolResult, value), strings.Contains(run.reply, value))
	}
	t.Logf("check 7 (redact, Read tool): value in tool result=%v, in reply=%v; placeholder in tool result=%v, in reply=%v; hook rows=%s", strings.Contains(run.toolResult, value), strings.Contains(run.reply, value), strings.Contains(run.toolResult, redactProbeHolder), strings.Contains(run.reply, redactProbeHolder), redactProbeRowSummary(p.rows()[before:]))
	p.findings("check 7", run, value)
	p.ledgerRow("check 7", "Read", "redact")

	value = p.newValue()
	before = len(p.rows())
	run = p.turn("report", "Read", readPrompt)
	counted = -1
	for _, row := range p.rows()[before:] {
		if row["event"] == "PostToolUse" && row["tool"] == "Read" {
			counted = int(row["count"].(float64))
		}
	}
	if !strings.Contains(run.toolResult, value) || counted != 1 {
		t.Errorf("check 8: report mode on Read: value in tool result=%v, hook counted %d", strings.Contains(run.toolResult, value), counted)
	}
	t.Logf("check 8 (report, Read tool): output unchanged=%v, hook counted=%d", strings.Contains(run.toolResult, value), counted)
	p.findings("check 8", run, value)
	p.ledgerRow("check 8", "Read", "report")

	p.sandboxOnly()
	p.noValueInWorkingTree()
}

// redactProbeRowSummary names each hook row's event, tool and response keys.
func redactProbeRowSummary(rows []map[string]any) string {
	var parts []string
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("%v/%v count=%v keys=%v", row["event"], row["tool"], row["count"], row["responseKeys"]))
	}
	return strings.Join(parts, "; ")
}

func newRedactProbe(t *testing.T) *redactProbe {
	t.Helper()
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "tt-redact-probe-")
	if err != nil {
		t.Fatal(err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	p := &redactProbe{t: t, root: root, project: filepath.Join(root, "project"), claude: claude, setting: filepath.Join(root, "setting"), log: filepath.Join(root, "hook.jsonl"), started: time.Now(), transcripts: map[string]bool{}}
	t.Cleanup(p.cleanup)
	if err := os.MkdirAll(filepath.Join(p.project, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	tmux := filepath.Join(root, "tmux")
	if err := os.Mkdir(tmux, 0700); err != nil {
		t.Fatal(err)
	}
	helper := []string{redactProbeSettingEnv + "=" + p.setting, redactProbeLogEnv + "=" + p.log, testTmuxDirEnv + "=" + tmux, "TMUX_TMPDIR=" + tmux}
	command := "env " + redactProbeHelperEnv + "=hook"
	for _, kv := range helper {
		command += " " + spawn.ShellQuote(kv)
	}
	command += " " + spawn.ShellQuote(self) + " -test.run='^TestRedactProbeHelper$'"
	// The freshly built tt runs beside the helper, as the installed one does.
	built := filepath.Join(root, "bin", "tt")
	if out, err := exec.Command("go", "build", "-o", built, ".").CombinedOutput(); err != nil {
		t.Fatalf("build tt: %v %s", err, out)
	}
	entry := []any{map[string]any{"hooks": []any{
		map[string]any{"type": "command", "command": spawn.ShellQuote(built) + " hook tool", "timeout": 5},
		map[string]any{"type": "command", "command": command, "timeout": 5},
	}}}
	settings, _ := json.MarshalIndent(map[string]any{"hooks": map[string]any{"PostToolUse": entry, "PostToolUseFailure": entry}}, "", "  ")
	if err := os.WriteFile(filepath.Join(p.project, ".claude", "settings.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}

	// The session's environment is built from nothing. HOME is the real one,
	// because Claude Code needs its own login; nothing else real is passed.
	token := "ttprobe" + redactProbeRandom(t, 40)
	p.values = append(p.values, token)
	identity := strings.ToLower(redactProbeRandom(t, 16))
	p.env = append([]string{
		"PATH=" + os.Getenv("PATH"), "TERM=" + os.Getenv("TERM"), "HOME=" + home,
		"TAILTERM_TOOL_LEDGER_DIR=" + filepath.Join(root, "ledger"), "TAILTERM_RELAY_STATE=" + filepath.Join(root, "relay"),
		"TAILTERM_AGENT=agt_probe" + identity, "TAILTERM_TASK=tsk_probe" + identity, "TAILTERM_RUN=run_probe" + identity,
		"TAILTERM_HUB=http://127.0.0.1:9", "TAILTERM_TOKEN=" + token, "TAILTERM_REDACT_SETTING_FILE=" + p.relayJSON(),
	}, helper...)

	// Pre-start assertion 1: no listed name carries a value from this process.
	child := map[string]string{}
	for _, kv := range p.env {
		name, value, _ := strings.Cut(kv, "=")
		child[name] = value
	}
	parent := map[string]string{}
	for _, kv := range redactProbeStartEnv {
		name, value, _ := strings.Cut(kv, "=")
		parent[name] = value
	}
	for _, name := range redactProbeListedEnv {
		if value, ok := child[name]; ok && (name != "TAILTERM_TOKEN" || value == parent[name]) {
			t.Fatalf("pre-start 1: the session's environment carries %s from outside the probe", name)
		}
	}
	// Pre-start assertions 2 and 3: what tt resolves from that environment.
	cmd := exec.Command(self, "-test.run=^TestRedactProbeHelper$")
	cmd.Env = append(append([]string{}, p.env...), redactProbeHelperEnv+"=resolve")
	out, err := cmd.Output()
	var resolved map[string]string
	if err != nil || json.Unmarshal(out, &resolved) != nil {
		t.Fatalf("pre-start: the resolve helper did not answer: %v", err)
	}
	for _, name := range []string{"ledger", "relay", "setting"} {
		if !strings.HasPrefix(resolved[name], root+string(filepath.Separator)) {
			t.Fatalf("pre-start 2: tt resolves its %s path outside the sandbox", name)
		}
	}
	if resolved["tokenDigest"] != fmt.Sprintf("%x", sha256.Sum256([]byte(token))) {
		t.Fatal("pre-start 3: the token tt loads is not the generated one")
	}
	t.Logf("pre-start assertions hold: no listed name inherited; ledger, relay and setting paths inside the sandbox; loaded token is the generated one")
	return p
}

func (p *redactProbe) newValue() string {
	p.t.Helper()
	value := "ghp_" + redactProbeRandom(p.t, 36)
	p.values = append(p.values, value)
	if err := os.WriteFile(filepath.Join(p.project, "value.txt"), []byte(value+"\n"), 0600); err != nil {
		p.t.Fatal(err)
	}
	return value
}

func (p *redactProbe) relayJSON() string { return filepath.Join(p.root, "relay.json") }

// ledgerRow requires that the built tt's last ledger row for the tool has
// exactly this mode and one github-token match, and names no skip.
func (p *redactProbe) ledgerRow(check, tool, mode string) {
	p.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(p.root, "ledger", "*", "ledger.jsonl"))
	var last map[string]any
	for _, path := range matches {
		data, _ := os.ReadFile(path)
		for _, line := range bytes.Split(data, []byte("\n")) {
			var row map[string]any
			if json.Unmarshal(line, &row) == nil && row["tool"] == tool && row["redactMode"] != nil {
				last = row
			}
		}
	}
	kinds, _ := last["redactKinds"].(map[string]any)
	if last["redactMode"] != mode || last["redactCount"] != float64(1) || len(kinds) != 1 || kinds["github-token"] != float64(1) || last["redactSkip"] != nil {
		p.t.Errorf("%s: ledger row has mode %v, count %v, kinds %v, skip %v", check, last["redactMode"], last["redactCount"], kinds, last["redactSkip"])
	}
	p.t.Logf("%s ledger row: redactMode=%v redactCount=%v redactKinds=%v", check, last["redactMode"], last["redactCount"], kinds)
}

func (p *redactProbe) rows() []map[string]any {
	data, _ := os.ReadFile(p.log)
	var rows []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		var row map[string]any
		if json.Unmarshal(line, &row) == nil && row != nil {
			rows = append(rows, row)
		}
	}
	return rows
}

// session runs one disposable `claude -p` turn that runs one Bash command.
func (p *redactProbe) session(mode, command string) redactProbeRun {
	p.t.Helper()
	return p.turn(mode, "Bash", "Run exactly this one Bash command, once, and use no other tool: "+command+"\nIt may exit non-zero; do not retry it. Then reply with the command's output copied exactly, character for character, and nothing else.")
}

// turn runs one disposable `claude -p` turn that is allowed one tool.
func (p *redactProbe) turn(mode, tool, prompt string) redactProbeRun {
	p.t.Helper()
	// Redact and report are the built tt's modes; the helper then only logs.
	// The other modes are the helper's misbehaviours, with tt switched off.
	helper, setting := mode, toolRedactOff
	if mode == toolRedactRedact || mode == toolRedactReport {
		helper, setting = "observe", mode
	}
	config, _ := json.Marshal(map[string]string{"claudeSecretRedaction": setting})
	if err := os.WriteFile(p.setting, []byte(helper), 0600); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(p.relayJSON(), config, 0600); err != nil {
		p.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.claude, "-p", prompt, "--output-format", "stream-json", "--verbose", "--allowedTools", tool, "--model", "haiku")
	cmd.Dir, cmd.Env = p.project, p.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		p.t.Fatal(err)
	}
	var run redactProbeRun
	var called time.Time
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var line struct {
			Type    string `json:"type"`
			Session string `json:"session_id"`
			Result  string `json:"result"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		if line.Session != "" {
			run.session = line.Session
		}
		if line.Type == "result" {
			run.reply = line.Result
		}
		var blocks []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"is_error"`
		}
		_ = json.Unmarshal(line.Message.Content, &blocks)
		for _, block := range blocks {
			switch block.Type {
			case "tool_use":
				if called.IsZero() {
					called = time.Now()
				}
			case "tool_result":
				if run.toolWait == 0 && !called.IsZero() {
					run.toolWait = time.Since(called)
				}
				run.toolResult += string(block.Content) + "\n"
				run.isError = run.isError || block.IsError
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		p.t.Fatalf("mode %s: the disposable session failed: %v (%d bytes of stderr withheld)", mode, err, stderr.Len())
	}
	if run.session == "" || run.toolResult == "" {
		p.t.Fatalf("mode %s: the disposable session ran no tool call", mode)
	}
	return run
}

// findings records which local files hold the generated value after a run.
func (p *redactProbe) findings(check string, run redactProbeRun, value string) {
	p.t.Helper()
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", run.session+".jsonl"))
	if len(matches) != 1 {
		p.t.Errorf("%s: found %d transcript files for the disposable session", check, len(matches))
		return
	}
	p.transcripts[filepath.Dir(matches[0])] = true
	data, err := os.ReadFile(matches[0])
	if err != nil {
		p.t.Errorf("%s: transcript: %v", check, err)
		return
	}
	var where []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.Contains(line, []byte(value)) {
			continue
		}
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			where = append(where, "unparsed line")
			continue
		}
		var paths []string
		redactProbePaths(row, "", value, &paths)
		where = append(where, fmt.Sprintf("%v row: %s", row["type"], strings.Join(paths, ", ")))
	}
	p.t.Logf("%s transcript: generated value present=%v, placeholder present=%v, where=%q", check, len(where) > 0, bytes.Contains(data, []byte(redactProbeHolder)), where)

	// Other local copies: files Claude Code wrote during the probe.
	var others []string
	for _, dir := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json"), filepath.Join(home, "Library", "Caches", "claude-cli-nodejs"), "/private/tmp/claude-501"} {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || path == matches[0] {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.ModTime().Before(p.started) || info.Size() > 64<<20 {
				return nil
			}
			if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(value)) {
				others = append(others, strings.ReplaceAll(strings.TrimPrefix(path, home), run.session, "SESSION"))
			}
			return nil
		})
	}
	p.t.Logf("%s other local copies holding the generated value: %q", check, others)
}

// redactProbePaths lists the JSON paths of the strings that hold the value.
func redactProbePaths(v any, at, value string, out *[]string) {
	switch x := v.(type) {
	case string:
		if strings.Contains(x, value) {
			*out = append(*out, at)
		}
	case []any:
		for i := range x {
			redactProbePaths(x[i], fmt.Sprintf("%s[%d]", at, i), value, out)
		}
	case map[string]any:
		for key := range x {
			redactProbePaths(x[key], at+"."+key, value, out)
		}
	}
}

// sandboxOnly checks that the installed `tt hook tool`, which the real home's
// settings also run in the disposable session, wrote under the sandbox only.
func (p *redactProbe) sandboxOnly() {
	p.t.Helper()
	agent := ""
	for _, kv := range p.env {
		if value, ok := strings.CutPrefix(kv, "TAILTERM_AGENT="); ok {
			agent = value
		}
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "tailterm", "tool-ledger", agent)); err == nil {
		p.t.Error("the real tool ledger gained a directory for the probe's agent")
	}
	_, err := os.Stat(filepath.Join(p.root, "ledger", agent, "ledger.jsonl"))
	p.t.Logf("installed tt hook wrote its ledger inside the sandbox=%v; real ledger untouched for the probe's agent", err == nil)
}

// noValueInWorkingTree runs before cleanup: nothing generated for this run
// is anywhere in the repository working tree.
func (p *redactProbe) noValueInWorkingTree() {
	p.t.Helper()
	patterns := filepath.Join(p.root, "patterns")
	if err := os.WriteFile(patterns, []byte(strings.Join(p.values, "\n")+"\n"), 0600); err != nil {
		p.t.Fatal(err)
	}
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		p.t.Fatal(err)
	}
	cmd := exec.Command("git", "grep", "-l", "-I", "--untracked", "-F", "-f", patterns)
	cmd.Dir = strings.TrimSpace(string(top))
	out, err := cmd.Output()
	exit, _ := err.(*exec.ExitError)
	if len(out) != 0 || exit == nil || exit.ExitCode() != 1 {
		p.t.Errorf("a generated value is in the working tree, or the search failed: %v, files %q", err, out)
		return
	}
	p.t.Logf("working tree search before cleanup: none of the %d generated values found", len(p.values))
}

// cleanup removes the sandbox and the disposable sessions' transcript folder,
// pass or fail, and requires both to be gone.
func (p *redactProbe) cleanup() {
	home, _ := os.UserHomeDir()
	projects := filepath.Join(home, ".claude", "projects")
	// Claude Code names the folder after the project path. Sessions that
	// failed before their id was read are found by that name.
	if matches, err := filepath.Glob(filepath.Join(projects, "*"+filepath.Base(p.root)+"-project")); err == nil {
		for _, dir := range matches {
			p.transcripts[dir] = true
		}
	}
	for dir := range p.transcripts {
		if filepath.Dir(dir) != projects || !strings.Contains(filepath.Base(dir), filepath.Base(p.root)) {
			p.t.Errorf("not removing an unexpected transcript folder %s", dir)
			continue
		}
		_ = os.RemoveAll(dir)
		if _, err := os.Stat(dir); err == nil {
			p.t.Errorf("the disposable transcript folder remains: %s", dir)
		}
	}
	_ = os.RemoveAll(p.root)
	if _, err := os.Stat(p.root); err == nil {
		p.t.Errorf("the sandbox root remains: %s", p.root)
	}
	p.t.Logf("cleanup: sandbox root and %d disposable transcript folder(s) removed", len(p.transcripts))
}

// ---- Unit tests. Every value below is generated at random for the run. ----

// redactSandbox is the ledger sandbox with the setting file under a temp
// directory, holding the given mode ("" writes no file), and every listed
// environment variable empty.
func redactSandbox(t *testing.T, mode string) (env, string) {
	t.Helper()
	e, root := toolLedgerSandbox(t)
	path := filepath.Join(t.TempDir(), "relay.json")
	if mode != "" {
		data, _ := json.Marshal(map[string]string{"claudeSecretRedaction": mode})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TAILTERM_REDACT_SETTING_FILE", path)
	for _, name := range toolRedactEnv {
		t.Setenv(name, "")
	}
	return e, root
}

// redactShapeCase pins one group B shape to the design's bounds,
// independently of the table the scanner uses.
type redactShapeCase struct {
	name, prefix, alphabet string
	n                      int
	exact                  bool
}

const redactUpperAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

var redactShapeCases = []redactShapeCase{
	{"github-token", "ghp_", redactAlnum, 36, true},
	{"github-token", "gho_", redactAlnum, 36, true},
	{"github-token", "ghu_", redactAlnum, 36, true},
	{"github-token", "ghs_", redactAlnum, 36, true},
	{"github-token", "ghr_", redactAlnum, 36, true},
	{"github-token", "github_pat_", redactAlnum + "_", 82, true},
	{"anthropic-key", "sk-ant-", redactAlnum + "_-", 80, false},
	{"openai-key", "sk-proj-", redactAlnum + "_-", 40, false},
	{"openai-key", "sk-", redactAlnum, 40, false},
	{"aws-access-key", "AKIA", redactUpperAlphabet, 16, true},
	{"aws-access-key", "ASIA", redactUpperAlphabet, 16, true},
	{"slack-token", "xoxa-", redactAlnum + "-", 10, false},
	{"slack-token", "xoxb-", redactAlnum + "-", 10, false},
	{"slack-token", "xoxp-", redactAlnum + "-", 10, false},
	{"slack-token", "xoxr-", redactAlnum + "-", 10, false},
	{"slack-token", "xoxs-", redactAlnum + "-", 10, false},
	{"tailscale-key", "tskey-auth-", redactAlnum + "-", 20, false},
	{"tailscale-key", "tskey-api-", redactAlnum + "-", 20, false},
	{"tailscale-key", "tskey-client-", redactAlnum + "-", 20, false},
	{"stripe-key", "sk_live_", redactAlnum, 24, false},
	{"stripe-key", "rk_live_", redactAlnum, 24, false},
	{"npm-token", "npm_", redactAlnum, 36, true},
}

func redactRandomFrom(t *testing.T, alphabet string, n int) string {
	t.Helper()
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(raw)
}

// make returns a synthetic value of the shape with a body of n characters.
// The body starts and ends with a letter or digit, as real ones do.
func (c redactShapeCase) make(t *testing.T, n int) string {
	t.Helper()
	body := []byte(redactRandomFrom(t, c.alphabet, n))
	inner := redactAlnum
	if c.alphabet == redactUpperAlphabet {
		inner = redactUpperAlphabet
	}
	body[0], body[n-1] = redactRandomFrom(t, inner, 1)[0], redactRandomFrom(t, inner, 1)[0]
	return c.prefix + string(body)
}

func redactText(t *testing.T, s string, exact ...string) string {
	t.Helper()
	out, ok := redactRewriteRaw(redactJSON(t, s), exact, time.Now().Add(time.Minute))
	if !ok {
		t.Fatal("the rewrite ran out of time")
	}
	var text string
	if err := json.Unmarshal(out, &text); err != nil {
		t.Fatalf("the rewritten string is not JSON: %v", err)
	}
	return text
}

// redactJSON encodes a value as Claude Code would send it, without Go's
// HTML escaping.
func redactJSON(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func redactCountValue(t *testing.T, v any) redactResult {
	t.Helper()
	return redactCountRaw(redactJSON(t, v), nil, time.Now().Add(time.Minute))
}

// requireRedact fails without printing either string: a failure names the
// case only.
func requireRedact(t *testing.T, label string, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: the rewritten text is not the expected one (lengths %d and %d)", label, len(got), len(want))
	}
}

// a10: every group B recognizer against the design's listed cases.
func TestToolRedactGroupBCases(t *testing.T) {
	if len(redactShapeCases) != len(redactShapes) {
		t.Fatalf("the scanner has %d shapes and the test pins %d", len(redactShapes), len(redactShapeCases))
	}
	for i, c := range redactShapeCases {
		if s := redactShapes[i]; s.name != c.name || s.prefix != c.prefix || s.min != c.n || s.exact != c.exact || s.noDash != (c.prefix == "sk-") {
			t.Fatalf("shape %d is %s %q %d %v; the design says %s %q %d %v", i, s.name, s.prefix, s.min, s.exact, c.name, c.prefix, c.n, c.exact)
		}
		hold := "[tt-redacted:" + c.name + "]"
		label := c.name + " " + c.prefix
		value := c.make(t, c.n)
		requireRedact(t, label+" well-formed", redactText(t, "before "+value+" after"), "before "+hold+" after")
		requireRedact(t, label+" whole string", redactText(t, value), hold)
		requireRedact(t, label+" at the start", redactText(t, value+"\nrest"), hold+"\nrest")
		requireRedact(t, label+" at the end", redactText(t, "first\n"+value), "first\n"+hold)
		short := c.make(t, c.n-1)
		requireRedact(t, label+" one short", redactText(t, "x "+short+" y"), "x "+short+" y")
		glued := "a" + value
		requireRedact(t, label+" after a letter", redactText(t, glued), glued)
		glued = "_" + value
		requireRedact(t, label+" after an underscore", redactText(t, glued), glued)
		if glued = "-" + value; c.prefix == "sk-" {
			requireRedact(t, label+" after a dash", redactText(t, glued), glued)
		} else {
			requireRedact(t, label+" after a dash", redactText(t, glued), "-"+hold)
		}
		long := c.make(t, c.n+1)
		if c.exact {
			requireRedact(t, label+" one long", redactText(t, "x "+long+" y"), "x "+long+" y")
		} else {
			requireRedact(t, label+" one long", redactText(t, "x "+long+" y"), "x "+hold+" y")
			run := c.make(t, c.n+3000)
			requireRedact(t, label+" long run", redactText(t, "x "+run+" y"), "x "+hold+" y")
			run = c.make(t, toolRedactLongest+500)
			requireRedact(t, label+" run past 4 KiB", redactText(t, run+"\n"), hold+"\n")
		}
		other := c.make(t, c.n)
		requireRedact(t, label+" two adjacent", redactText(t, value+","+other), hold+","+hold)
		requireRedact(t, label+" two on two lines", redactText(t, value+"\n"+other+"\n"), hold+"\n"+hold+"\n")
		// Two overlapping matches: a host value that starts inside the token
		// and runs past it. One span, named after the leftmost.
		tail := "!" + redactRandomFrom(t, redactAlnum, 12)
		host := value[len(value)-8:] + tail
		requireRedact(t, label+" two overlapping", redactText(t, "x "+value+tail+" y", host), "x "+hold+" y")
	}
}

func TestToolRedactPrivateKey(t *testing.T) {
	const hold = "[tt-redacted:private-key]"
	for _, kind := range []string{"", "RSA ", "EC ", "OPENSSH ", "ENCRYPTED "} {
		label := kind + "PRIVATE KEY"
		begin, end := "-----BEGIN "+label+"-----", "-----END "+label+"-----"
		var body strings.Builder
		for i := 0; i < 6; i++ {
			body.WriteString(redactRandomFrom(t, redactAlnum+"+/", 64) + "\n")
		}
		key := begin + "\n" + body.String() + end
		requireRedact(t, label+" complete", redactText(t, "key:\n"+key+"\nnext line"), "key:\n"+hold+"\nnext line")
		requireRedact(t, label+" whole string", redactText(t, key), hold)
		// With no end line, nothing after the header survives.
		requireRedact(t, label+" no end line", redactText(t, "key:\n"+begin+"\n"+body.String()+"more text"), "key:\n"+hold)
		requireRedact(t, label+" two keys", redactText(t, key+"\n"+key), hold+"\n"+hold)
	}
	// A public key and a header that is too long are not private keys.
	for _, text := range []string{
		"-----BEGIN " + "PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----",
		"-----BEGIN " + strings.Repeat("A", 45) + " PRIVATE KEY" + "-----\nabc",
		"-----BEGIN " + "private key-----\nabc",
	} {
		requireRedact(t, "not a private key", redactText(t, text), text)
	}
}

// Group A: values the host holds, each only when at least 16 characters.
func TestToolRedactHostValues(t *testing.T) {
	long, short := redactRandomFrom(t, redactAlnum+"-_.", 40), redactRandomFrom(t, redactAlnum, 15)
	const hold = "[tt-redacted:host-credential]"
	requireRedact(t, "long host value", redactText(t, "TOKEN="+long+"\nx="+long, long), "TOKEN="+hold+"\nx="+hold)
	// A short value is never compared: toolRedactExact leaves it out.
	for _, name := range toolRedactEnv {
		t.Setenv(name, "")
	}
	t.Setenv("GH_TOKEN", long)
	t.Setenv("TS_API_KEY", short)
	t.Setenv("AWS_SECRET_ACCESS_KEY", redactRandomFrom(t, redactAlnum, 40)) // not a listed name
	token := redactRandomFrom(t, redactAlnum, 32)
	if got := toolRedactExact(token); len(got) != 2 || got[0] != token || got[1] != long {
		t.Fatalf("host values compared: %d, want the hub token and the one long listed variable", len(got))
	}
}

// Group C is counted and never rewritten, and is not counted where a group
// A or B match already covers it.
func TestToolRedactGroupCCountsOnly(t *testing.T) {
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]string{"sub": redactRandomFrom(t, redactAlnum, 12)})
	jwt := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims) + "." + redactRandomFrom(t, redactAlnum+"_-", 43)
	discord := "M" + redactRandomFrom(t, redactAlnum, 23) + "." + redactRandomFrom(t, redactAlnum, 6) + "." + redactRandomFrom(t, redactAlnum, 30)
	for name, text := range map[string]string{
		"assigned-secret": "api_key = \"" + redactRandomFrom(t, redactAlnum, 32) + "\"",
		"bearer":          "Authorization: Bearer " + redactRandomFrom(t, redactAlnum, 30),
		"jwt":             "cookie " + jwt,
		"url-password":    "postgres://user:" + redactRandomFrom(t, redactAlnum, 18) + "@db.example:5432/app",
		"aws-secret-key":  "aws_secret_access_key=" + redactRandomFrom(t, redactAlnum+"/+", 40),
		"google-api-key":  "key " + "AIza" + redactRandomFrom(t, redactAlnum, 35),
		"discord-token":   "bot " + discord,
	} {
		found := redactCountValue(t, map[string]any{"stdout": text})
		if found.timeout || found.rewrite != 0 || len(found.kinds) != 1 || found.kinds[name] != 1 {
			t.Errorf("%s: kinds %v, rewriting matches %d; want that one name counted once", name, found.kinds, found.rewrite)
		}
		requireRedact(t, name+" is not rewritten", redactText(t, text), text)
	}
	// Not secrets: a short value, a repeated character, a plain URL.
	for _, text := range []string{"password: hunter2", "token=" + strings.Repeat("x", 40), "https://example.com/a:b@c", "secret_count: 12"} {
		if found := redactCountValue(t, text); found.count() != 0 {
			t.Errorf("a non-secret was counted: kinds %v", found.kinds)
		}
	}
	// A named GitHub token is one github-token, not also an assigned-secret.
	c := redactShapeCases[0]
	if found := redactCountValue(t, "token: "+c.make(t, c.n)); found.count() != 1 || found.kinds["github-token"] != 1 || found.rewrite != 1 {
		t.Errorf("a named token was counted as %v", found.kinds)
	}
}

// redactPost runs one PostToolUse hook and returns its stdout and the row.
func redactPost(t *testing.T, e env, root string, response any) (string, map[string]any) {
	t.Helper()
	fields := map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_use_id": "u" + redactRandomFrom(t, redactAlnum, 8), "tool_input": map[string]any{"command": "true"}}
	if response != nil {
		fields["tool_response"] = response
	}
	r := runToolHook(t, e, "tool", toolPayload("PostToolUse", fields), nil)
	if r.err != nil {
		t.Fatalf("hook error %v", r.err)
	}
	rows := toolLedgerRows(t, root, e.agent)
	if len(rows) == 0 {
		t.Fatal("no ledger row")
	}
	return r.out, rows[len(rows)-1]
}

func requireNoRedactFields(t *testing.T, label string, row map[string]any) {
	t.Helper()
	for key := range row {
		if strings.HasPrefix(key, "redact") {
			t.Errorf("%s: the row has %s", label, key)
		}
	}
}

// a2 and a11: report mode is complete on its own. For every input the hook
// prints nothing and the rewrite is never called.
func TestToolRedactReportMode(t *testing.T) {
	e, root := redactSandbox(t, toolRedactReport)
	setToolLedger(t, &toolRedactRewrite, func([]byte, []string, time.Time) ([]byte, bool) {
		t.Error("report mode called the rewrite")
		return nil, false
	})
	c := redactShapeCases[0]
	value := c.make(t, c.n)
	out, row := redactPost(t, e, root, map[string]any{"stdout": "line\n" + value + "\n", "stderr": "", "interrupted": false})
	if out != "" || row["redactMode"] != "report" || row["redactCount"] != float64(1) || !reflect.DeepEqual(row["redactKinds"], map[string]any{"github-token": float64(1)}) || row["redactSkip"] != nil || row["outcome"] != "ok" {
		t.Fatalf("report row: stdout %d bytes, mode %v count %v kinds %v skip %v", len(out), row["redactMode"], row["redactCount"], row["redactKinds"], row["redactSkip"])
	}
	// Nothing matched: the mode and a zero count, no kinds.
	out, row = redactPost(t, e, root, map[string]any{"stdout": "nothing here", "stderr": ""})
	if _, has := row["redactKinds"]; out != "" || row["redactCount"] != float64(0) || has {
		t.Fatalf("clean output: stdout %d bytes, count %v, kinds present %v", len(out), row["redactCount"], has)
	}
	// Every shape, group C, a host value, every output form, and each skip.
	host := redactRandomFrom(t, redactAlnum, 30)
	e.token = host
	var all []any
	want := map[string]any{"host-credential": float64(1), "private-key": float64(1), "bearer": float64(1)}
	for _, c := range redactShapeCases {
		all = append(all, c.make(t, c.n))
		count, _ := want[c.name].(float64)
		want[c.name] = count + 1
	}
	all = append(all, "hub "+host, "-----BEGIN "+"PRIVATE KEY-----\n"+redactRandomFrom(t, redactAlnum, 64), "Authorization: Bearer "+redactRandomFrom(t, redactAlnum, 30))
	inputs := []any{all, strings.Join([]string{all[0].(string), all[5].(string)}, " "), map[string]any{"file": map[string]any{"content": all[0], "numLines": 1}, "type": "text"}, 12, true, []any{}, map[string]any{}}
	for i, input := range inputs {
		out, row := redactPost(t, e, root, input)
		if out != "" || row["redactMode"] != "report" || row["redactSkip"] != nil {
			t.Fatalf("input %d: stdout %d bytes, mode %v, skip %v", i, len(out), row["redactMode"], row["redactSkip"])
		}
		if i == 0 && (!reflect.DeepEqual(row["redactKinds"], want) || row["redactCount"] != float64(len(all))) {
			t.Fatalf("all shapes: count %v kinds %v, want %d and %v", row["redactCount"], row["redactKinds"], len(all), want)
		}
	}
	if out, row := redactPost(t, e, root, nil); out != "" || row["redactSkip"] != "unreadable" || row["redactCount"] != nil {
		t.Fatalf("no tool_response: stdout %d bytes, skip %v", len(out), row["redactSkip"])
	}
	setToolLedger(t, &toolRedactMaxBytes, 64)
	if out, row := redactPost(t, e, root, map[string]any{"stdout": value + strings.Repeat(" ", 64)}); out != "" || row["redactSkip"] != "oversize" || row["redactCount"] != nil {
		t.Fatalf("oversize: stdout %d bytes, skip %v", len(out), row["redactSkip"])
	}
	setToolLedger(t, &toolRedactMaxBytes, 512<<10)
	setToolLedger(t, &toolRedactScanBudget, 0)
	if out, row := redactPost(t, e, root, map[string]any{"stdout": value}); out != "" || row["redactSkip"] != "timeout" || row["redactCount"] != nil {
		t.Fatalf("timeout: stdout %d bytes, skip %v", len(out), row["redactSkip"])
	}
	requireNoValueInLedger(t, root, e.agent, append([]string{host}, redactStrings(all)...)...)
}

func redactStrings(values []any) []string {
	var out []string
	for _, v := range values {
		out = append(out, v.(string))
	}
	return out
}

// requireNoValueInLedger: no ledger or pending file holds a generated value.
func requireNoValueInLedger(t *testing.T, root, agent string, values ...string) {
	t.Helper()
	_ = filepath.WalkDir(filepath.Join(root, agent), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(path)
		for i, value := range values {
			if bytes.Contains(data, []byte(value)) {
				t.Errorf("%s holds generated value %d", filepath.Base(path), i)
			}
		}
		return nil
	})
}

// a3: redact mode answers with one JSON object whose updated output has the
// input's exact structure, only the value replaced.
func TestToolRedactRedactMode(t *testing.T) {
	e, root := redactSandbox(t, toolRedactRedact)
	c := redactShapeCases[0]
	value := c.make(t, c.n)
	const hold = "[tt-redacted:github-token]"
	big := json.Number("12345678901234567890")
	for label, form := range map[string]func(string) any{
		"string": func(v string) any { return "out: " + v + " <&> end" },
		"object": func(v string) any {
			return map[string]any{"stdout": "a\n" + v + "\nb", "stderr": "", "interrupted": false, "isImage": false, "code": 0, "big": big, "nothing": nil,
				"nested": map[string]any{"list": []any{1.5, "keep", v, map[string]any{v: "a key is structure and is kept"}}}}
		},
		"array": func(v string) any {
			return []any{map[string]any{"type": "text", "text": "x " + v}, map[string]any{"type": "text", "text": "clean"}, 7, nil, []any{v}}
		},
	} {
		out, row := redactPost(t, e, root, form(value))
		dec := json.NewDecoder(strings.NewReader(out))
		dec.UseNumber()
		var answer map[string]map[string]any
		if err := dec.Decode(&answer); err != nil || dec.More() || strings.Count(out, "\n") != 1 {
			t.Fatalf("%s: stdout is not one JSON object on one line (%d bytes): %v", label, len(out), err)
		}
		specific := answer["hookSpecificOutput"]
		if len(answer) != 1 || len(specific) != 3 || specific["hookEventName"] != "PostToolUse" {
			t.Fatalf("%s: answer keys %d and %d, event %v", label, len(answer), len(specific), specific["hookEventName"])
		}
		// The expected output is the input with the placeholder in the
		// value's place, taken through the same JSON encoding.
		encoded, _ := json.Marshal(form("\x00"))
		wantDec := json.NewDecoder(strings.NewReader(strings.ReplaceAll(string(encoded), `\u0000`, hold)))
		wantDec.UseNumber()
		var want any
		if err := wantDec.Decode(&want); err != nil {
			t.Fatal(err)
		}
		if label == "object" {
			// The key that is the value itself stays: keys are not output.
			list := want.(map[string]any)["nested"].(map[string]any)["list"].([]any)
			list[3] = map[string]any{value: "a key is structure and is kept"}
		}
		if !reflect.DeepEqual(specific["updatedToolOutput"], want) {
			t.Fatalf("%s: the updated output is not the input with only the value replaced", label)
		}
		notice, _ := specific["additionalContext"].(string)
		if !strings.Contains(notice, "Do not write a placeholder into any file") || strings.Contains(notice, value) {
			t.Fatalf("%s: notice %q", label, notice)
		}
		if row["redactMode"] != "redact" || row["redactSkip"] != nil || row["redactKinds"].(map[string]any)["github-token"] == nil {
			t.Fatalf("%s: row mode %v skip %v kinds %v", label, row["redactMode"], row["redactSkip"], row["redactKinds"])
		}
	}
	// Nothing to replace, or group C only: no answer at all.
	for label, response := range map[string]any{
		"clean":   map[string]any{"stdout": "plain output"},
		"group C": map[string]any{"stdout": "Authorization: Bearer " + redactRandomFrom(t, redactAlnum, 30)},
	} {
		if out, row := redactPost(t, e, root, response); out != "" || row["redactMode"] != "redact" || row["redactSkip"] != nil {
			t.Fatalf("%s: stdout %d bytes, mode %v, skip %v", label, len(out), row["redactMode"], row["redactSkip"])
		}
	}
	// A rewrite that would change the shape is not sent.
	setToolLedger(t, &toolRedactRewrite, func(raw []byte, _ []string, _ time.Time) ([]byte, bool) {
		return append(append([]byte("["), raw...), ']'), true
	})
	if out, row := redactPost(t, e, root, map[string]any{"stdout": value}); out != "" || row["redactSkip"] != "shape" {
		t.Fatalf("shape: stdout %d bytes, skip %v", len(out), row["redactSkip"])
	}
	setToolLedger(t, &toolRedactRewrite, func([]byte, []string, time.Time) ([]byte, bool) { return nil, false })
	if out, row := redactPost(t, e, root, map[string]any{"stdout": value}); out != "" || row["redactSkip"] != "timeout" {
		t.Fatalf("late rewrite: stdout %d bytes, skip %v", len(out), row["redactSkip"])
	}
	requireNoValueInLedger(t, root, e.agent, value)
}

func TestToolRedactSameShape(t *testing.T) {
	a := map[string]any{"s": "x", "n": json.Number("1"), "l": []any{"y", nil, true}}
	for label, b := range map[string]any{
		"retyped string": map[string]any{"s": 1, "n": json.Number("1"), "l": []any{"y", nil, true}},
		"changed number": map[string]any{"s": "x", "n": json.Number("2"), "l": []any{"y", nil, true}},
		"missing key":    map[string]any{"s": "x", "l": []any{"y", nil, true}},
		"renamed key":    map[string]any{"t": "x", "n": json.Number("1"), "l": []any{"y", nil, true}},
		"shorter list":   map[string]any{"s": "x", "n": json.Number("1"), "l": []any{"y", nil}},
		"a list":         []any{a},
	} {
		if redactSameShape(a, b) {
			t.Errorf("%s counted as the same shape", label)
		}
	}
	if !redactSameShape(a, map[string]any{"s": "other", "n": json.Number("1"), "l": []any{"z", nil, true}}) {
		t.Error("changed string text counted as a different shape")
	}
}

// Redaction applies to successful calls in sessions the ledger identifies,
// and to nothing else.
func TestToolRedactOnlyIdentifiedSuccessfulCalls(t *testing.T) {
	e, root := redactSandbox(t, toolRedactRedact)
	c := redactShapeCases[0]
	value := c.make(t, c.n)
	fields := func(id string) map[string]any {
		return map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_use_id": id, "tool_input": map[string]any{"command": "true"}, "tool_response": map[string]any{"stdout": value}, "error": value}
	}
	toolHook(t, e, "PreToolUse", fields("u1"))
	toolHook(t, e, "PostToolUseFailure", fields("u1"))
	for _, row := range toolLedgerRows(t, root, e.agent) {
		requireNoRedactFields(t, "failed call", row)
	}
	// No Tailterm identity and no owner helper: the owner's other sessions.
	toolHook(t, env{}, "PostToolUse", fields("u2"))
	toolHook(t, env{hub: e.hub, agent: e.agent}, "PostToolUse", fields("u3")) // no task
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != 1 {
		t.Fatalf("%d rows, want the one failed call", len(rows))
	}
}

// a4: the hook returns within its deadline with stdin held open and with
// stdout blocked, and a scan past its budget prints nothing.
func TestToolRedactTimeBounds(t *testing.T) {
	e, root := redactSandbox(t, toolRedactRedact)
	c := redactShapeCases[0]
	t.Run("stdin held open", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		if run := runToolHook(t, e, "tool", "", r); run.err != nil || run.out != "" || run.elapsed > 200*time.Millisecond {
			t.Fatalf("held stdin: err %v, %d bytes out, %s", run.err, len(run.out), run.elapsed)
		}
	})
	t.Run("stdout blocked", func(t *testing.T) {
		// A pipe nobody reads, and an answer larger than a pipe holds. The
		// scan's own budget is lifted so a slow host still reaches the write.
		setToolLedger(t, &toolRedactScanBudget, time.Minute)
		var lines strings.Builder
		for lines.Len() < 256<<10 {
			lines.WriteString(c.make(t, c.n) + "\n")
		}
		payload := toolPayload("PostToolUse", map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_use_id": "blocked", "tool_input": map[string]any{}, "tool_response": map[string]any{"stdout": lines.String()}})
		if len(payload) > toolRedactMaxBytes {
			t.Fatalf("the payload of %d bytes is over the size cap", len(payload))
		}
		path := filepath.Join(t.TempDir(), "stdin")
		if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		stdin, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer stdin.Close()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		oldIn, oldOut := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = stdin, w
		start := time.Now()
		runErr := cmdHook(e, []string{"tool"})
		elapsed := time.Since(start)
		os.Stdin, os.Stdout = oldIn, oldOut
		before := len(toolLedgerRows(t, root, e.agent))
		// Reading now lets the abandoned write finish, so the goroutine ends.
		got := make(chan int)
		go func() { n, _ := io.Copy(io.Discard, r); got <- int(n) }()
		deadline := time.Now().Add(5 * time.Second)
		for len(toolLedgerRows(t, root, e.agent)) == before && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		w.Close()
		wrote := <-got
		r.Close()
		if runErr != nil || elapsed > 200*time.Millisecond || elapsed < toolLedgerDeadline {
			t.Fatalf("blocked stdout: err %v, returned after %s; want nil at the %s deadline", runErr, elapsed, toolLedgerDeadline)
		}
		if before != 0 || wrote <= 64<<10 {
			t.Fatalf("blocked stdout: %d rows before the pipe was read, %d bytes written; the write was not blocked", before, wrote)
		}
	})
	t.Run("scan past its budget", func(t *testing.T) {
		setToolLedger(t, &toolRedactScanBudget, 0)
		out, row := redactPost(t, e, root, map[string]any{"stdout": c.make(t, c.n)})
		if out != "" || row["redactSkip"] != "timeout" || row["redactMode"] != "redact" {
			t.Fatalf("stdout %d bytes, skip %v", len(out), row["redactSkip"])
		}
	})
	t.Run("deadline inside a scan", func(t *testing.T) {
		if _, _, ok := redactScan(strings.Repeat("plain text ", 1000), nil, time.Now().Add(-time.Second)); ok {
			t.Fatal("a scan past its deadline reported a complete result")
		}
	})
}

// The setting: only the exact key with report or redact switches it on.
func TestToolRedactSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	t.Setenv("TAILTERM_REDACT_SETTING_FILE", path)
	if got := toolRedactMode(); got != toolRedactOff {
		t.Fatalf("missing file: %s", got)
	}
	for content, want := range map[string]string{
		`{"claudeSecretRedaction":"report"}`:                                  toolRedactReport,
		`{"claudeSecretRedaction":"redact","claudeStallAction":"interrupt"}`:  toolRedactRedact,
		`{"claudeSecretRedaction":"off"}`:                                     toolRedactOff,
		`{"claudeSecretRedaction":"Redact"}`:                                  toolRedactOff,
		`{"claudeSecretRedaction":"on"}`:                                      toolRedactOff,
		`{"claudeSecretRedaction":true}`:                                      toolRedactOff,
		`{"claudesecretredaction":"redact"}`:                                  toolRedactOff,
		`{"claudesecretredaction":"redact","claudeSecretRedaction":"report"}`: toolRedactReport,
		`{"ClaudeSecretRedaction":"redact"}`:                                  toolRedactOff,
		`{"claudeStallAction":"interrupt"}`:                                   toolRedactOff,
		`{"claudeSecretRedaction":"redact"`:                                   toolRedactOff,
		``:                                                                    toolRedactOff,
		`{"claudeSecretRedaction":"redact","pad":"` + strings.Repeat("x", 70<<10) + `"}`: toolRedactOff,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if got := toolRedactMode(); got != want {
			t.Errorf("setting file of %d bytes starting %.40q: %s, want %s", len(content), content, got, want)
		}
	}
	// Without the test override it is relay.json beside hub.json.
	t.Setenv("TAILTERM_REDACT_SETTING_FILE", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := toolRedactSettingPath(), filepath.Join(home, ".config", "tailterm", "relay.json"); got != want {
		t.Fatalf("setting path %s, want %s", got, want)
	}
}

// a5: the labelled corpus. Every generated positive is replaced, and group B
// matches nothing in the repository's tracked files or the look-alikes.
func TestToolRedactLabelledCorpus(t *testing.T) {
	far := time.Now().Add(time.Hour)
	for _, c := range redactShapeCases {
		value := c.make(t, c.n)
		for label, text := range map[string]string{
			"env line": "export KEY=" + value + "\n", "json": `{"key":"` + value + `","n":1}`, "yaml": "key: '" + value + "'\n",
			"sentence": "the value is " + value + ".", "header": "X-Key: " + value + "\r\n", "url": "https://example.com/?k=" + value + "&x=1",
		} {
			got := redactText(t, text)
			if strings.Contains(got, value) || strings.Count(got, "[tt-redacted:"+c.name+"]") != 1 {
				t.Errorf("%s %s in a %s was not replaced once", c.name, c.prefix, label)
			}
		}
	}
	// Negatives: generated look-alikes. They are not secrets, so they come
	// from a fixed seed: the same 18,000 strings on every run, and a result
	// that cannot change from run to run.
	const hex = "0123456789abcdef"
	seeded := mathrand.New(mathrand.NewSource(79001641))
	pick := func(alphabet string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[seeded.Intn(len(alphabet))]
		}
		return string(b)
	}
	checked := 0
	for i := 0; i < 2000; i++ {
		uuid := pick(hex, 8) + "-" + pick(hex, 4) + "-4" + pick(hex, 3) + "-a" + pick(hex, 3) + "-" + pick(hex, 12)
		raw := make([]byte, 3000)
		_, _ = seeded.Read(raw)
		for label, text := range map[string]string{
			"git hash": "commit " + pick(hex, 40), "short hash": pick(hex, 7) + " Fix it",
			"sha256": "sha256:" + pick(hex, 64), "uuid": "session " + uuid,
			"base64 image": "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw), "base64url": base64.RawURLEncoding.EncodeToString(raw[:600]),
			"message number": fmt.Sprintf("#%d and #%d", 28000+i, 29000+i), "work item": "wi_" + pick(hex, 16),
			"ids": "tsk_" + pick(hex, 16) + " agt_" + pick(hex, 16) + " run_" + pick(hex, 16) + " mpr_" + pick(hex, 16),
		} {
			checked++
			if rewrite, _, _ := redactScan(text, nil, far); len(rewrite) != 0 {
				t.Errorf("group B matched a generated %s as %s", label, rewrite[0].name)
			}
		}
	}
	// Negatives: every tracked file, each scanned as one string.
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("no git checkout: %v", err)
	}
	root := strings.TrimSpace(string(top))
	list, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	// Four tracked files hold deliberate secret-shaped fixtures of other
	// features: made-up values written to look like the real thing. They are
	// pinned by pattern and count, so a new match in them still fails.
	fixtures := map[string]string{
		"client/main.js":                               "private-key 1;",                                               // a text box placeholder: the header alone
		"hub/cmd/tt/handoff_test.go":                   "private-key 1;",                                               // a PEM block the handoff record must never store
		"hub/internal/bridge/helper_test.go":           "aws-access-key 2;github-token 1;private-key 1;slack-token 2;", // made-up secrets the bridge must scrub; the key header hides the rest of the file
		"hub/internal/jev/testdata/redact_corpus.json": "github-token 1;",                                              // the message checker's own redaction corpus
	}
	files := 0
	for _, name := range strings.Split(strings.TrimRight(string(list), "\x00"), "\x00") {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue // a tracked file deleted in the working tree
		}
		files++
		rewrite, _, _ := redactScan(string(data), nil, far)
		got := ""
		kinds := map[string]int{}
		for _, span := range rewrite {
			kinds[span.name]++
		}
		for _, kind := range redactGroupBNames() {
			if n := kinds[kind]; n > 0 {
				got += fmt.Sprintf("%s %d;", kind, n)
			}
		}
		if got != fixtures[name] {
			t.Errorf("group B matches in tracked file %s: %q, want %q", name, got, fixtures[name])
		}
		delete(fixtures, name)
	}
	if len(fixtures) != 0 {
		t.Errorf("pinned fixture files not seen: %v", fixtures)
	}
	if files < 100 {
		t.Fatalf("only %d tracked files read", files)
	}
	t.Logf("labelled corpus: %d shapes replaced in 6 contexts each; zero group B matches in %d look-alikes and in %d tracked files apart from 4 pinned fixture files", len(redactShapeCases), checked, files)
}

// a6: run with TT_REDACT_BENCH=1 on the host being measured. A 100 KiB
// output must add under 20 ms at the 95th percentile, and an output of the
// size cap must be scanned, and in redact mode rewritten, within 50 ms. The
// outputs measured are ordinary source text, the worst cases above, an
// output that is nothing but tokens, and one of many small strings.
func TestToolRedactBenchmark(t *testing.T) {
	if os.Getenv("TT_REDACT_BENCH") != "1" {
		t.Skip("requires an explicit benchmark run")
	}
	source, err := os.ReadFile("tool_ledger_test.go")
	if err != nil {
		t.Fatal(err)
	}
	c := redactShapeCases[0]
	texts := map[string]string{"source text": string(source), "tokens only": c.make(t, c.n) + "\n"}
	for _, unit := range redactWorstCases {
		texts[fmt.Sprintf("%q", unit)] = unit
	}
	p95 := func(response any, mode string, runs int) time.Duration {
		data := []byte(toolPayload("PostToolUse", map[string]any{"tool_name": "Bash", "tool_response": response}))
		times := make([]time.Duration, runs)
		for i := range times {
			l := toolLedger{started: time.Now(), redactBudget: time.Hour, redactMax: 64 << 20, redactMode: mode, stdout: io.Discard}
			var row toolLedgerRow
			l.redact(&row, data)
			times[i] = time.Since(l.started)
			if row.RedactSkip != "" || row.RedactCount == nil {
				t.Fatalf("benchmark scan skipped: %s", row.RedactSkip)
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		return times[len(times)*95/100]
	}
	fill := func(unit string, size int) any {
		return map[string]any{"stdout": strings.Repeat(unit, size/len(unit)+1)[:size], "stderr": "", "interrupted": false}
	}
	var small, capped time.Duration
	var smallName, cappedName string
	measure := func(name string, at100, atCap any) {
		for _, mode := range []string{toolRedactReport, toolRedactRedact} {
			if d := p95(at100, mode, 100); d > small {
				small, smallName = d, name+", "+mode
			}
			if d := p95(atCap, mode, 40); d > capped {
				capped, cappedName = d, name+", "+mode
			}
		}
	}
	for name, unit := range texts {
		measure(name, fill(unit, 100<<10), fill(unit, toolRedactMaxBytes-1024))
	}
	many := func(size int) any {
		var list []any
		for n := 0; n < size; n += 24 {
			list = append(list, "a short line of output")
		}
		return list
	}
	measure("many small strings", many(100<<10), many(toolRedactMaxBytes-1024))
	plain := p95(fill(string(source), 100<<10), toolRedactReport, 200)
	t.Logf("p95: 100 KiB of source text, report mode, %s; slowest 100 KiB output %s (%s); slowest output of the %d KiB cap %s (%s)", plain, small, smallName, toolRedactMaxBytes>>10, capped, cappedName)
	if small >= 20*time.Millisecond {
		t.Errorf("a 100 KiB output adds %s at the 95th percentile (%s), want under 20 ms", small, smallName)
	}
	if capped >= 50*time.Millisecond {
		t.Errorf("an output of the size cap takes %s at the 95th percentile (%s), want under 50 ms", capped, cappedName)
	}
}

// a9 and a13: host setup on a host that already has the tool hooks leaves
// the Claude settings byte-identical, and never creates or changes
// relay.json. Only the hook's reader names the setting.
func TestToolRedactHostSetupLeavesSettingsAndRelayJSON(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	artifact := s.artifact("tt-v1")
	s.mustRun("--from", artifact)
	relayJSON := filepath.Join(s.home, ".config", "tailterm", "relay.json")
	if _, err := os.Stat(relayJSON); err == nil {
		t.Fatal("host setup created relay.json")
	}
	settings, err := os.ReadFile(s.p.claudeSettings)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"} {
		if !strings.Contains(string(settings), `"`+event+`"`) || strings.Count(string(settings), "hook tool") != 3 {
			t.Fatalf("the sandbox home does not hold the three tool hook entries (%s)", event)
		}
	}
	const owner = `{"claudeStallAction":"report","claudeSecretRedaction":"report"}` + "\n"
	s.write(relayJSON, owner, 0o600)
	s.mustRun("--from", artifact)
	s.mustRun("--from", s.artifact("tt-v2"))
	after, err := os.ReadFile(s.p.claudeSettings)
	if err != nil || !bytes.Equal(settings, after) {
		t.Fatalf("host setup changed the Claude settings (%v)", err)
	}
	if kept, err := os.ReadFile(relayJSON); err != nil || string(kept) != owner {
		t.Fatalf("host setup changed relay.json (%v)", err)
	}
	// No other tt source names the setting, and its reader only reads.
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sources {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if name != "tool_redact.go" && !strings.HasSuffix(name, "_test.go") && bytes.Contains(data, []byte("claudeSecretRedaction")) {
			t.Errorf("%s names the redaction setting", name)
		}
		if name == "tool_redact.go" {
			for _, write := range []string{"os.WriteFile", "os.Create", "os.OpenFile", "os.Rename", "os.Remove", "os.Mkdir", "writePrivateJSON"} {
				if bytes.Contains(data, []byte(write)) {
					t.Errorf("tool_redact.go calls %s", write)
				}
			}
		}
	}
}

// redactWorstCases are texts built to make a careless scan slow: a prefix
// that is refused again and again inside one long run, a name whose value
// runs to the end of the text, and the like.
var redactWorstCases = []string{
	"_sk-ant-", "_tskey-auth-", "token=", "plain text ", "-sk-", "asecret:", "aaaatoken=", "-eyJ", "eyJ.", ".aaaaaa", "AIza", "://", "://a:b",
	"-----BEGIN ", "-----BEGIN " + "PRIVATE KEY-----", "aws_secret_access_key=", "authorization: bearer ", "password: '", "xoxb-", "_ghp_", "api_key=x",
}

// Review b1: every loop of the scan is linear and honours the deadline. Each
// worst case, up to the size cap, is scanned well inside the 100 ms budget,
// and a scan whose deadline has passed returns at once.
func TestToolRedactScanIsBounded(t *testing.T) {
	exact := []string{strings.Repeat("ab", 10), "token=token=token=token="}
	slowest, slowestUnit := time.Duration(0), ""
	for _, unit := range redactWorstCases {
		for _, size := range []int{64 << 10, 256 << 10, toolRedactMaxBytes} {
			text := strings.Repeat(unit, size/len(unit))
			start := time.Now()
			_, _, ok := redactScan(text, exact, start.Add(100*time.Millisecond))
			elapsed := time.Since(start)
			if elapsed > slowest {
				slowest, slowestUnit = elapsed, fmt.Sprintf("%q at %d KiB", unit, size>>10)
			}
			if size == toolRedactMaxBytes && testing.Verbose() {
				t.Logf("%q at the cap: %s", unit, elapsed.Round(100*time.Microsecond))
			}
			if elapsed > 130*time.Millisecond {
				t.Errorf("unit %q, %d KiB: the scan returned after %s, ok=%v; its budget is 100 ms", unit, size>>10, elapsed.Round(time.Millisecond), ok)
			}
			start = time.Now()
			if _, _, ok := redactScan(text, exact, start.Add(-time.Second)); ok || time.Since(start) > 30*time.Millisecond {
				t.Errorf("unit %q, %d KiB: a scan already past its deadline returned ok=%v after %s", unit, size>>10, ok, time.Since(start).Round(time.Millisecond))
			}
		}
	}
	t.Logf("slowest worst-case scan: %s for %s", slowest.Round(100*time.Microsecond), slowestUnit)
}

// Review b1, at the hook: a generated value on the first line followed by a
// worst-case text up to the cap still leaves a row, with its count or with
// the timeout skip, and the hook returns inside its deadline.
func TestToolRedactWorstCaseLeavesARow(t *testing.T) {
	c := redactShapeCases[0]
	for _, mode := range []string{toolRedactReport, toolRedactRedact} {
		e, root := redactSandbox(t, mode)
		for _, unit := range []string{"_sk-ant-", "token=", "plain text "} {
			for _, size := range []int{64 << 10, 256 << 10, 500 << 10} {
				value := c.make(t, c.n)
				before := len(toolLedgerRows(t, root, e.agent))
				fields := map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_use_id": redactRandomFrom(t, redactAlnum, 8), "tool_input": map[string]any{},
					"tool_response": map[string]any{"stdout": value + "\n" + strings.Repeat(unit, size/len(unit))}}
				payload := toolPayload("PostToolUse", fields)
				label := fmt.Sprintf("%s, %q, %d KiB", mode, unit, size>>10)
				// The ledger decodes its input before any scan starts. Where
				// that alone takes a large part of the 150 ms, as under the
				// race detector, this size cannot leave a row on this run.
				decodeStart := time.Now()
				var probe toolHookInput
				_ = json.Unmarshal([]byte(payload), &probe)
				if spent := time.Since(decodeStart); spent > 25*time.Millisecond {
					t.Logf("%s: not run, decoding the input alone took %s here", label, spent.Round(time.Millisecond))
					continue
				}
				r := runToolHook(t, e, "tool", payload, nil)
				rows := toolLedgerRows(t, root, e.agent)
				if r.err != nil || r.elapsed > 200*time.Millisecond || len(rows) != before+1 {
					t.Fatalf("%s: err %v, %s, %d new rows; want one row inside 200 ms", label, r.err, r.elapsed.Round(time.Millisecond), len(rows)-before)
				}
				row := rows[len(rows)-1]
				if row["redactSkip"] == "timeout" {
					if r.out != "" {
						t.Fatalf("%s: a timed-out scan printed %d bytes", label, len(r.out))
					}
					continue // a loaded host: the stated fail-open result
				}
				kinds, _ := row["redactKinds"].(map[string]any)
				if row["redactSkip"] != nil || kinds["github-token"] != float64(1) {
					t.Fatalf("%s: row skip %v, kinds %v; want the value counted", label, row["redactSkip"], kinds)
				}
				if mode == toolRedactRedact && (strings.Contains(r.out, value) || !strings.Contains(r.out, "[tt-redacted:github-token]")) {
					t.Fatalf("%s: the answer of %d bytes does not replace the value", label, len(r.out))
				}
			}
		}
	}
}

// Review f1: a redact answer is the output as it was written, byte for byte,
// with only the value cut out. A lone surrogate escape, other escapes,
// spacing, key order and number forms all pass through untouched.
func TestToolRedactAnswerPreservesBytes(t *testing.T) {
	e, _ := redactSandbox(t, toolRedactRedact)
	c := redactShapeCases[0]
	value := c.make(t, c.n)
	const hold = "[tt-redacted:github-token]"
	for label, raw := range map[string]string{
		"lone surrogate":     `{"stdout":"pre \ud83d mid ` + value + ` end","stderr":""}`,
		"escapes and spaces": `{ "z" : 1.50, "a":"x\/y \u00e9 \ud83d\ude00 \"q\" \\ \t<&> ` + value + `\n", "big":12345678901234567890 ,"e":1e3, "k\u0065y":[ "` + value + `" , null ] }`,
		"a bare string":      `"\u0041 ` + value + ` \udc00"`,
		"value at both ends": `["` + value + `\n` + value + `"]`,
	} {
		// The payload is written by hand: json.Marshal would rewrite the
		// raw output's escapes before the hook ever saw them.
		input := `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash","tool_use_id":"u1","tool_input":{},"tool_response":` + raw + `}`
		r := runToolHook(t, e, "tool", input, nil)
		want := `{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":` + strings.ReplaceAll(raw, value, hold) + `,"additionalContext":`
		if r.err != nil || !strings.HasPrefix(r.out, want) || strings.Contains(r.out, "\ufffd") {
			t.Errorf("%s: the answer is not the output with only the value replaced (%d bytes, err %v)", label, len(r.out), r.err)
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(r.out), &answer); err != nil {
			t.Errorf("%s: the answer is not JSON: %v", label, err)
		}
	}
	// A value that is itself written with escapes is still found, and the
	// whole of its written form is cut out.
	escaped := `\u0067hp\u005f` + value[4:len(value)-1] + fmt.Sprintf(`\u%04x`, value[len(value)-1])
	raw := `{"stdout":"a ` + escaped + ` b \ud83d"}`
	r := runToolHook(t, e, "tool", `{"hook_event_name":"PostToolUse","session_id":"s1","tool_name":"Bash","tool_use_id":"u2","tool_input":{},"tool_response":`+raw+`}`, nil)
	if want := `"updatedToolOutput":{"stdout":"a ` + hold + ` b \ud83d"},`; !strings.Contains(r.out, want) {
		t.Errorf("an escaped value was not cut out whole (%d bytes)", len(r.out))
	}
}

// redactDecode reads a string token as encoding/json does, and its offsets
// point at the written form of each decoded byte.
func TestToolRedactDecode(t *testing.T) {
	for _, token := range []string{
		`""`, `"plain"`, `"caf\u00e9"`, `"\ud83d\ude00 pair"`, `"lone \ud83d high"`, `"lone \ude00 low"`, `"\ud83d\u0041"`, `"\ud83d"`,
		`"\" \\ \/ \b \f \n \r \t"`, `"é raw and \u00E9 escaped"`, `"end \\"`, `"\u0000"`,
	} {
		var want string
		if err := json.Unmarshal([]byte(token), &want); err != nil {
			t.Fatalf("%s: %v", token, err)
		}
		got, offsets := redactDecode([]byte(token))
		if got != want {
			t.Errorf("%s decoded to %q, want %q", token, got, want)
		}
		if offsets == nil {
			continue
		}
		if len(offsets) != len(got)+1 || int(offsets[len(got)]) != len(token)-1 {
			t.Errorf("%s: %d offsets for %d bytes", token, len(offsets), len(got))
		}
		for i := 1; i < len(offsets); i++ {
			if offsets[i] < offsets[i-1] || offsets[i] < 1 {
				t.Errorf("%s: offsets go backwards at %d", token, i)
			}
		}
	}
	// Keys are skipped, values are found, in order.
	var found []string
	raw := []byte(`{"k1":"v1","k\"2":{"k3":["v2",{"k4":"v\"3"},"v4"],"k5":"v5"},"k6":7,"k7":[["v6"]]}`)
	if !redactEachString(raw, func(start, end int) bool { found = append(found, string(raw[start:end])); return true }) {
		t.Fatal("the walk stopped")
	}
	if want := []string{`"v1"`, `"v2"`, `"v\"3"`, `"v4"`, `"v5"`, `"v6"`}; !reflect.DeepEqual(found, want) {
		t.Errorf("string values found: %q, want %q", found, want)
	}
}
