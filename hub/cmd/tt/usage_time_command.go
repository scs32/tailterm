package main

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
)

// Time accounting (docs/usage-accounting.md, Time) classifies a tool call by
// the shell commands it executes, never by a pattern over its raw input. Only
// the class and the inbox-only flag leave this file; command text does not.
const (
	usageToolOrdinary = "tool"    // tool time; also every call that cannot be decoded
	usageToolWaiting  = "waiting" // a blocking inbox wait beside cd and tt commands only
	usageToolMixed    = "mixed"   // a blocking inbox wait beside other work: unmeasured
	usageToolInbox    = "inbox"   // non-blocking inbox checks only; still tool time
)

type usageToolClass struct {
	Class     string
	InboxOnly bool
}

var usageUnclassified = usageToolClass{Class: usageToolOrdinary}

// classifyUsageTool decodes one tool call of either runtime. kind is the Codex
// response item type ("function_call" or "custom_tool_call"); Claude passes
// "tool_use". payload is the call's arguments, input or command object.
func classifyUsageTool(runtime, kind, name string, payload json.RawMessage) usageToolClass {
	commands, ok := usageToolCommands(runtime, kind, name, payload)
	if !ok || len(commands) == 0 {
		return usageUnclassified
	}
	return classifyUsageCommands(commands)
}

func usageToolCommands(runtime, kind, name string, payload json.RawMessage) ([]string, bool) {
	if runtime == "claude" {
		if name != "Bash" {
			return nil, false
		}
		var input struct {
			Command *string `json:"command"`
		}
		if json.Unmarshal(payload, &input) != nil || input.Command == nil {
			return nil, false
		}
		return []string{*input.Command}, true
	}
	if kind == "custom_tool_call" {
		if name != "exec" {
			return nil, false
		}
		var snippet string
		if json.Unmarshal(payload, &snippet) != nil {
			return nil, false
		}
		return usageExecSnippetCommands(snippet)
	}
	// function_call arguments are a JSON object, or a JSON string holding one.
	var encoded string
	if json.Unmarshal(payload, &encoded) == nil {
		payload = json.RawMessage(encoded)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return nil, false
	}
	for _, key := range []string{"cmd", "command"} {
		raw, present := fields[key]
		if !present {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return []string{text}, true
		}
		var argv []string
		if json.Unmarshal(raw, &argv) != nil {
			return nil, false
		}
		for i, arg := range argv {
			if (arg == "-c" || arg == "-lc") && i+1 < len(argv) {
				return []string{argv[i+1]}, true
			}
		}
		return nil, false
	}
	return nil, false
}

// usageExecSnippetCommands is a small lexer over a Codex exec JavaScript
// snippet. It skips string, template and comment tokens and collects the cmd
// of every tools.exec_command({...}) call. Any other tools.NAME( call, a cmd
// that is not a plain string literal, or an unterminated token fails.
func usageExecSnippetCommands(src string) ([]string, bool) {
	var commands []string
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, false
			}
			i += end + 4
		case c == '"' || c == '\'' || c == '`':
			_, _, next, ok := usageJSString(src, i)
			if !ok {
				return nil, false
			}
			i = next
		case usageJSIdentStart(c):
			start := i
			for i < len(src) && usageJSIdentPart(src[i]) {
				i++
			}
			if src[start:i] != "tools" || (start > 0 && src[start-1] == '.') {
				continue
			}
			j := usageJSSpace(src, i)
			if j >= len(src) || src[j] != '.' {
				continue
			}
			j = usageJSSpace(src, j+1)
			nameStart := j
			for j < len(src) && usageJSIdentPart(src[j]) {
				j++
			}
			name := src[nameStart:j]
			j = usageJSSpace(src, j)
			if j >= len(src) || src[j] != '(' {
				continue
			}
			if name != "exec_command" {
				return nil, false
			}
			cmd, next, ok := usageExecCommandArgument(src, usageJSSpace(src, j+1))
			if !ok {
				return nil, false
			}
			commands = append(commands, cmd)
			i = next
		default:
			i++
		}
	}
	return commands, true
}

func usageJSIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func usageJSIdentPart(c byte) bool { return usageJSIdentStart(c) || (c >= '0' && c <= '9') }
func usageJSSpace(src string, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i++
	}
	return i
}

// usageJSString reads the literal starting at src[i]. plain is false for a
// template that interpolates or a literal with an undecodable escape; the decoded
// value is only meaningful when plain.
func usageJSString(src string, i int) (value string, plain bool, next int, ok bool) {
	quote := src[i]
	var out strings.Builder
	plain = true
	for j := i + 1; j < len(src); j++ {
		c := src[j]
		switch {
		case c == '\\':
			if j+1 >= len(src) {
				return "", false, 0, false
			}
			j++
			switch src[j] {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case '\n':
			case 'x', 'u':
				digits := 2
				if src[j] == 'u' {
					digits = 4
				}
				if j+digits >= len(src) {
					return "", false, 0, false
				}
				code, err := strconv.ParseUint(src[j+1:j+1+digits], 16, 32)
				if err != nil {
					plain = false // \u{...} and malformed escapes are not trusted
				} else {
					out.WriteRune(rune(code))
					j += digits
				}
			default:
				out.WriteByte(src[j])
			}
		case c == quote:
			return out.String(), plain, j + 1, true
		case c == '\n' && quote != '`':
			return "", false, 0, false
		case quote == '`' && c == '$' && j+1 < len(src) && src[j+1] == '{':
			plain = false
			depth := 0
			for ; j < len(src); j++ {
				if src[j] == '{' {
					depth++
				} else if src[j] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if j >= len(src) {
				return "", false, 0, false
			}
		default:
			out.WriteByte(c)
		}
	}
	return "", false, 0, false
}

// usageExecCommandArgument reads the object literal at src[i] and returns its
// cmd when that is a plain string literal.
func usageExecCommandArgument(src string, i int) (string, int, bool) {
	if i >= len(src) || src[i] != '{' {
		return "", 0, false
	}
	i++
	cmd, found := "", false
	for {
		i = usageJSSpace(src, i)
		if i >= len(src) {
			return "", 0, false
		}
		if src[i] == '}' {
			i++
			break
		}
		var key string
		if src[i] == '"' || src[i] == '\'' {
			value, plain, next, ok := usageJSString(src, i)
			if !ok || !plain {
				return "", 0, false
			}
			key, i = value, next
		} else if usageJSIdentStart(src[i]) {
			start := i
			for i < len(src) && usageJSIdentPart(src[i]) {
				i++
			}
			key = src[start:i]
		} else {
			return "", 0, false // spread, computed key or anything else
		}
		i = usageJSSpace(src, i)
		if i >= len(src) || src[i] != ':' {
			return "", 0, false // shorthand property: the value is a variable
		}
		i = usageJSSpace(src, i+1)
		if key == "cmd" {
			if i >= len(src) || (src[i] != '"' && src[i] != '\'' && src[i] != '`') {
				return "", 0, false
			}
			value, plain, next, ok := usageJSString(src, i)
			if !ok || !plain || found {
				return "", 0, false
			}
			i = usageJSSpace(src, next)
			if i >= len(src) || (src[i] != ',' && src[i] != '}') {
				return "", 0, false // concatenation or a method call on the literal
			}
			cmd, found = value, true
		} else {
			// Skip one value: to the comma or closing brace at this depth.
			depth := 0
			for i < len(src) {
				c := src[i]
				if c == '"' || c == '\'' || c == '`' {
					_, _, next, ok := usageJSString(src, i)
					if !ok {
						return "", 0, false
					}
					i = next
					continue
				}
				if c == '{' || c == '[' || c == '(' {
					depth++
				} else if c == '}' || c == ']' || c == ')' {
					if depth == 0 {
						break
					}
					depth--
				} else if c == ',' && depth == 0 {
					break
				}
				i++
			}
		}
		if i >= len(src) {
			return "", 0, false
		}
		if src[i] == ',' {
			i++
		}
	}
	if !found {
		return "", 0, false
	}
	i = usageJSSpace(src, i)
	if i >= len(src) || src[i] != ')' {
		return "", 0, false
	}
	return cmd, i + 1, true
}

const (
	usageCommandOther = iota
	usageCommandCD
	usageCommandTT
	usageCommandInbox
	usageCommandWait
)

func classifyUsageCommands(commands []string) usageToolClass {
	blocking, other, total := false, false, 0
	inboxOnly := true
	for _, command := range commands {
		kinds, ok := usageShellCommandKinds(command, 0)
		if !ok {
			return usageUnclassified
		}
		leading := true
		for _, kind := range kinds {
			total++
			switch kind {
			case usageCommandWait:
				blocking, leading = true, false
			case usageCommandInbox:
				leading = false
			case usageCommandCD:
				if !leading {
					inboxOnly = false
				}
			case usageCommandTT:
				inboxOnly, leading = false, false
			default:
				other, inboxOnly, leading = true, false, false
			}
		}
		if leading {
			inboxOnly = false // a command string with no inbox command at all
		}
	}
	if total == 0 {
		return usageUnclassified
	}
	switch {
	case blocking && other:
		return usageToolClass{Class: usageToolMixed}
	case blocking:
		return usageToolClass{Class: usageToolWaiting, InboxOnly: inboxOnly}
	case inboxOnly:
		return usageToolClass{Class: usageToolInbox, InboxOnly: true}
	}
	return usageUnclassified
}

// usageShellCommandKinds tokenises one shell string with quote handling and
// splits it into simple commands at ; && || | & and newline. Command
// substitution, a heredoc or unbalanced quotes fail the whole string.
func usageShellCommandKinds(src string, depth int) ([]int, bool) {
	var kinds []int
	var words []string
	var word strings.Builder
	inWord := false
	flushWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	ok := true
	flushCommand := func() {
		flushWord()
		if len(words) > 0 {
			nested, valid := usageSimpleCommandKinds(words, depth)
			if !valid {
				ok = false
			}
			kinds = append(kinds, nested...)
		}
		words = nil
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch c {
		case '\'':
			end := strings.IndexByte(src[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			word.WriteString(src[i+1 : i+1+end])
			inWord = true
			i += end + 1
		case '"':
			closed := false
			for i++; i < len(src); i++ {
				d := src[i]
				if d == '"' {
					closed = true
					break
				}
				if d == '`' || (d == '$' && i+1 < len(src) && src[i+1] == '(') {
					return nil, false
				}
				if d == '\\' && i+1 < len(src) {
					i++
					d = src[i]
				}
				word.WriteByte(d)
			}
			if !closed {
				return nil, false
			}
			inWord = true
		case '\\':
			if i+1 >= len(src) {
				return nil, false
			}
			i++
			if src[i] != '\n' {
				word.WriteByte(src[i])
				inWord = true
			}
		case '`':
			return nil, false
		case '$':
			if i+1 < len(src) && src[i+1] == '(' {
				return nil, false
			}
			word.WriteByte(c)
			inWord = true
		case '<':
			if i+1 < len(src) && src[i+1] == '<' {
				return nil, false
			}
			word.WriteByte(c)
			inWord = true
		case '#':
			if inWord {
				word.WriteByte(c)
				continue
			}
			for i < len(src) && src[i] != '\n' {
				i++
			}
			i--
		case ' ', '\t', '\r':
			flushWord()
		case '\n', ';':
			flushCommand()
		case '|':
			flushCommand()
			if i+1 < len(src) && (src[i+1] == '|' || src[i+1] == '&') {
				i++
			}
		case '&':
			if i+1 < len(src) && src[i+1] == '&' {
				flushCommand()
				i++
				continue
			}
			// A redirection such as 2>&1 or &>file is part of its word.
			if (i > 0 && (src[i-1] == '>' || src[i-1] == '<')) || (i+1 < len(src) && src[i+1] == '>') {
				word.WriteByte(c)
				inWord = true
				continue
			}
			flushCommand()
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	flushCommand()
	return kinds, ok
}

func usageAssignmentWord(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq < 1 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := word[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// usageSimpleCommandKinds names one simple command. A bash|sh|zsh -c STRING
// recurses once into STRING and may return several kinds.
func usageSimpleCommandKinds(words []string, depth int) ([]int, bool) {
	for len(words) > 0 {
		w := words[0]
		switch {
		case usageAssignmentWord(w), w == "env", w == "command", w == "exec", w == "nohup", w == "time":
			words = words[1:]
			continue
		case w == "timeout":
			if len(words) < 2 {
				return []int{usageCommandOther}, true
			}
			words = words[2:]
			continue
		}
		break
	}
	if len(words) == 0 {
		return nil, true
	}
	base := filepath.Base(words[0])
	args := words[1:]
	switch base {
	case "bash", "sh", "zsh":
		if depth == 0 && len(args) == 2 && (args[0] == "-c" || args[0] == "-lc") {
			return usageShellCommandKinds(args[1], depth+1)
		}
		return []int{usageCommandOther}, true
	case "cd":
		return []int{usageCommandCD}, true
	case "tt":
	default:
		return []int{usageCommandOther}, true
	}
	if len(args) == 0 {
		return []int{usageCommandTT}, true
	}
	switch args[0] {
	case "wait":
		return []int{usageCommandWait}, true
	case "inbox":
		for _, arg := range args[1:] {
			if arg == "--wait" || strings.HasPrefix(arg, "--wait=") {
				return []int{usageCommandWait}, true
			}
		}
		return []int{usageCommandInbox}, true
	}
	return []int{usageCommandTT}, true
}
