package main

import (
	"encoding/json"
	"testing"
)

func claudeBash(command string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"command": command})
	return raw
}

// codexExec wraps each command in the snippet shape Codex writes.
func codexExec(commands ...string) json.RawMessage {
	snippet := ""
	for _, command := range commands {
		literal, _ := json.Marshal(command)
		snippet += "text(await tools.exec_command({cmd:" + string(literal) + ",max_output_tokens:2000}));\n"
	}
	raw, _ := json.Marshal(snippet)
	return raw
}
func codexFunction(command string, asString bool) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"cmd": command})
	if asString {
		raw, _ = json.Marshal(string(raw))
	}
	return raw
}

func TestUsageTimeCommandClassification(t *testing.T) {
	type want struct {
		class     string
		inboxOnly bool
	}
	check := func(t *testing.T, name string, got usageToolClass, w want) {
		t.Helper()
		if got.Class != w.class || got.InboxOnly != w.inboxOnly {
			t.Errorf("%s: got %+v, want %+v", name, got, w)
		}
	}
	// Every shell string is checked in each form that carries one command.
	shell := []struct {
		command string
		want    want
	}{
		{"tt inbox --unread --wait 9m", want{usageToolWaiting, true}},
		{"/Users/x/.local/bin/tt wait", want{usageToolWaiting, true}},
		{"cd /w && TAILTERM_X=1 timeout 600 tt inbox --wait=9m", want{usageToolWaiting, true}},
		{`bash -lc "tt wait"`, want{usageToolWaiting, true}},
		{"tt wait", want{usageToolWaiting, true}},
		{"tt inbox --unread --mark-read --wait 9m 2>&1", want{usageToolWaiting, true}},
		{"env TAILTERM_X=1 nohup tt wait", want{usageToolWaiting, true}},
		// Waiting, but not inbox-only (lead decision #19079).
		{"tt ack 5 && tt inbox --wait 9m", want{usageToolWaiting, false}},
		{"cd /w && tt progress 5 --text x; tt wait", want{usageToolWaiting, false}},
		{"tt wait; cd /w", want{usageToolWaiting, false}},
		// Mixed: timestamps cannot split the call.
		{"tt inbox --wait 9m && go test ./...", want{usageToolMixed, false}},
		{"git status; tt wait", want{usageToolMixed, false}},
		{"tt inbox --wait 9m | tail -5", want{usageToolMixed, false}},
		{"tt wait & sleep 5", want{usageToolMixed, false}},
		// Inbox check, not blocking.
		{"tt inbox --unread --mark-read", want{usageToolInbox, true}},
		{"cd /w && tt inbox --unread", want{usageToolInbox, true}},
		{"tt inbox; tt inbox --unread", want{usageToolInbox, true}},
		// Ordinary tool time.
		{"echo tt wait", want{usageToolOrdinary, false}},
		{`tt post "please run tt inbox --wait 9m"`, want{usageToolOrdinary, false}},
		{"tt send --text 'tt wait'", want{usageToolOrdinary, false}},
		{"mytt wait", want{usageToolOrdinary, false}},
		{"tt ack 5 && go test ./...", want{usageToolOrdinary, false}},
		{"tt waiting", want{usageToolOrdinary, false}},
		{"echo $(tt inbox --wait 9m)", want{usageToolOrdinary, false}},
		{"echo `tt wait`", want{usageToolOrdinary, false}},
		{"tt wait <<EOF\nx\nEOF", want{usageToolOrdinary, false}},
		{`tt wait "unbalanced`, want{usageToolOrdinary, false}},
		{"tt inbox --unread && tt ack 5", want{usageToolOrdinary, false}},
		{`bash -c "bash -c 'tt wait'"`, want{usageToolOrdinary, false}},
		{"# tt wait", want{usageToolOrdinary, false}},
		{"", want{usageToolOrdinary, false}},
		{"go test ./...", want{usageToolOrdinary, false}},
	}
	for _, c := range shell {
		check(t, "claude Bash: "+c.command, classifyUsageTool("claude", "tool_use", "Bash", claudeBash(c.command)), c.want)
		check(t, "codex exec: "+c.command, classifyUsageTool("codex", "custom_tool_call", "exec", codexExec(c.command)), c.want)
		check(t, "codex function_call: "+c.command, classifyUsageTool("codex", "function_call", "exec_command", codexFunction(c.command, false)), c.want)
		check(t, "codex function_call string: "+c.command, classifyUsageTool("codex", "function_call", "exec_command", codexFunction(c.command, true)), c.want)
		argv, _ := json.Marshal(map[string][]string{"command": {"bash", "-lc", c.command}})
		check(t, "codex function_call argv: "+c.command, classifyUsageTool("codex", "function_call", "shell", argv), c.want)
	}

	// Payload shapes that are not one shell string.
	raw := func(v any) json.RawMessage { out, _ := json.Marshal(v); return out }
	payloads := []struct {
		name                string
		runtime, kind, tool string
		payload             json.RawMessage
		want                want
	}{
		{"exact snippet from a rollout", "codex", "custom_tool_call", "exec", raw(`text(await tools.exec_command({cmd:"tt inbox --wait 9m",max_output_tokens:2000}));`), want{usageToolWaiting, true}},
		{"two exec_command calls, ack then wait", "codex", "custom_tool_call", "exec", codexExec("tt ack 5", "tt inbox --wait 9m"), want{usageToolWaiting, false}},
		{"two exec_command calls, wait and go test", "codex", "custom_tool_call", "exec", codexExec("tt inbox --wait 9m", "go test ./..."), want{usageToolMixed, false}},
		{"parallel exec_command calls", "codex", "custom_tool_call", "exec", raw("const r = await Promise.allSettled([\n  tools.exec_command({ cmd: 'tt inbox --unread', yield_time_ms: 1000 }),\n  tools.exec_command({ \"cmd\": \"tt inbox\" }),\n]);\ntext(r);"), want{usageToolInbox, true}},
		{"cmd is a template with interpolation", "codex", "custom_tool_call", "exec", raw("const d='9m'; text(await tools.exec_command({cmd:`tt inbox --wait ${d}`}));"), want{usageToolOrdinary, false}},
		{"cmd is a plain template", "codex", "custom_tool_call", "exec", raw("text(await tools.exec_command({cmd:`tt wait`}));"), want{usageToolWaiting, true}},
		{"cmd is a variable", "codex", "custom_tool_call", "exec", raw(`const cmd="tt wait"; text(await tools.exec_command({cmd}));`), want{usageToolOrdinary, false}},
		{"cmd is a concatenation", "codex", "custom_tool_call", "exec", raw(`text(await tools.exec_command({cmd:"tt wait"+x}));`), want{usageToolOrdinary, false}},
		{"another tool beside the wait", "codex", "custom_tool_call", "exec", raw(`await tools.apply_patch({input:"x"}); await tools.exec_command({cmd:"tt wait"});`), want{usageToolOrdinary, false}},
		{"wait text only inside a string", "codex", "custom_tool_call", "exec", raw(`text("tools.exec_command({cmd:'tt wait'})"); await tools.exec_command({cmd:"ls"});`), want{usageToolOrdinary, false}},
		{"wait text only inside a comment", "codex", "custom_tool_call", "exec", raw("// tools.exec_command({cmd:'tt wait'})\nawait tools.exec_command({cmd:\"ls\"});"), want{usageToolOrdinary, false}},
		{"snippet with no exec_command", "codex", "custom_tool_call", "exec", raw(`text("tt wait")`), want{usageToolOrdinary, false}},
		{"unterminated snippet string", "codex", "custom_tool_call", "exec", raw(`tools.exec_command({cmd:"tt wait`), want{usageToolOrdinary, false}},
		{"exec input that is not a string", "codex", "custom_tool_call", "exec", raw(map[string]string{"cmd": "tt wait"}), want{usageToolOrdinary, false}},
		{"custom tool other than exec", "codex", "custom_tool_call", "apply_patch", raw("tt wait"), want{usageToolOrdinary, false}},
		{"JSON arguments without cmd or command", "codex", "function_call", "sleep", raw(map[string]int{"duration_ms": 540000}), want{usageToolOrdinary, false}},
		{"JSON arguments with a non-string cmd", "codex", "function_call", "exec_command", raw(map[string]int{"cmd": 5}), want{usageToolOrdinary, false}},
		{"argv without -c", "codex", "function_call", "shell", raw(map[string][]string{"command": {"tt", "wait"}}), want{usageToolOrdinary, false}},
		{"undecodable arguments", "codex", "function_call", "exec_command", json.RawMessage(`{"cmd":`), want{usageToolOrdinary, false}},
		{"Claude tool other than Bash", "claude", "tool_use", "Read", raw(map[string]string{"command": "tt wait"}), want{usageToolOrdinary, false}},
		{"Claude Bash without a command", "claude", "tool_use", "Bash", raw(map[string]string{"description": "tt wait"}), want{usageToolOrdinary, false}},
	}
	for _, c := range payloads {
		check(t, c.name, classifyUsageTool(c.runtime, c.kind, c.tool, c.payload), c.want)
	}
}
