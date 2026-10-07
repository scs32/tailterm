package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/spawn"
)

// Secret redaction (feature wi_79001641d97cd64c), step one: the sandbox probe
// of the design's section 6. It is a throwaway. A disposable `claude -p`
// session in a temporary project runs this test binary as its PostToolUse and
// PostToolUseFailure hook, and the probe records what Claude Code does with
// each kind of hook answer. No product code is exercised yet.
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
			"ledger": toolLedgerRoot(), "relay": relayDir(), "setting": os.Getenv(redactProbeSettingEnv),
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
	switch string(mode) {
	case "redact":
		answer(updated)
		_, _ = os.Stdout.Write(out.Bytes())
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

// TestSecretRedactionSandboxProbe: run with TT_LIVE_CLAUDE=1. It starts six
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

	p.sandboxOnly()
	p.noValueInWorkingTree()
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
	entry := []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 5}}}}
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
		"TAILTERM_HUB=http://127.0.0.1:9", "TAILTERM_TOKEN=" + token,
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
	if err := os.WriteFile(p.setting, []byte(mode), 0600); err != nil {
		p.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	prompt := "Run exactly this one Bash command, once, and use no other tool: " + command + "\nIt may exit non-zero; do not retry it. Then reply with the command's output copied exactly, character for character, and nothing else."
	cmd := exec.CommandContext(ctx, p.claude, "-p", prompt, "--output-format", "stream-json", "--verbose", "--allowedTools", "Bash", "--model", "haiku")
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
