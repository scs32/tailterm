package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The would-deny report (feature wi_23ca1183c7d59c79) is report-only. On
// PreToolUse the tool-call ledger classifies a Bash command against four
// protected-operation rules and appends one would_deny row per matched rule.
// It blocks nothing and prints nothing, and any failure in the check writes
// nothing extra. It only reads the command text: no process is started, no
// file is read and the hub is never asked. See docs/claude-wake.md.

// Variables rather than constants so tests can change them. A hook call
// copies them once, before its goroutine starts.
var (
	toolDenyCheck      = toolLedger.wouldDeny
	toolDenyMaxCommand = 64 << 10 // a longer command is not classified at all
)

const (
	toolDenyMaxShells    = 3   // bash -c, eval and ssh commands classified inside one another
	toolDenyMaxNest      = 8   // substitutions inside one another
	toolDenyMaxParts     = 8   // summary words before the rest are only counted
	toolDenySummaryBytes = 120 // largest stored summary

	toolDenyHostPkill = "host-pkill"
	toolDenyDeployer  = "deployer-signal"
	toolDenyOldSite   = "old-site-deploy"
	toolDenyTrueNAS   = "truenas-tailscale"

	toolDenyDeployerPrefix = "tt-deployer"
)

// toolDenyRules is the order matches are returned in.
var toolDenyRules = []string{toolDenyHostPkill, toolDenyDeployer, toolDenyOldSite, toolDenyTrueNAS}

// toolDenyTrueNASAddrs holds the addresses that mean the TrueNAS host. A
// host name is recognised by containing "truenas". docs/handoff.md gives
// the address.
var toolDenyTrueNASAddrs = []string{"100.116.238.37"}

var toolDenySignals = map[string]bool{
	"HUP": true, "INT": true, "QUIT": true, "ILL": true, "TRAP": true, "ABRT": true, "EMT": true, "FPE": true,
	"KILL": true, "BUS": true, "SEGV": true, "SYS": true, "PIPE": true, "ALRM": true, "TERM": true, "URG": true,
	"STOP": true, "TSTP": true, "CONT": true, "CHLD": true, "TTIN": true, "TTOU": true, "IO": true, "XCPU": true,
	"XFSZ": true, "VTALRM": true, "PROF": true, "WINCH": true, "INFO": true, "USR1": true, "USR2": true,
}

// The tmux subcommands that end or type into a session, with their aliases.
var toolDenyTmuxCommands = map[string]string{
	"kill-session": "kill-session", "kill-window": "kill-window", "killw": "kill-window",
	"kill-pane": "kill-pane", "killp": "kill-pane", "send-keys": "send-keys", "send": "send-keys",
	"respawn-pane": "respawn-pane", "respawnp": "respawn-pane",
	"respawn-window": "respawn-window", "respawnw": "respawn-window",
}

var toolDenyTailscaleCommands = map[string]bool{
	"up": true, "down": true, "set": true, "status": true, "serve": true, "funnel": true,
	"login": true, "logout": true, "update": true, "cert": true, "ip": true,
}

// Package manager options that take the next word as their value.
var toolDenyNpmValued = map[string]bool{
	"--prefix": true, "-C": true, "--dir": true, "--cwd": true, "--filter": true, "-w": true, "--workspace": true,
}

var toolDenyNodeValued = map[string]bool{
	"-r": true, "--require": true, "--import": true, "--loader": true, "--experimental-loader": true,
	"--env-file": true, "-C": true, "--conditions": true, "--title": true, "--inspect-port": true,
}

// Long options of the wrapper commands that take the next word.
var toolDenyWrapperValued = map[string]bool{
	"--user": true, "--group": true, "--host": true, "--prompt": true, "--chdir": true, "--unset": true,
	"--signal": true, "--kill-after": true, "--max-args": true, "--max-procs": true, "--replace": true,
	"--delimiter": true, "--arg-file": true, "--max-lines": true, "--max-chars": true, "--eof": true,
}

// toolDenyMatch is one rule a command matched. parts are the words of its
// summary: fixed words the classifier recognised and a count of the rest,
// never text copied from the command.
type toolDenyMatch struct {
	rule, kind string
	parts      []string
}

// toolDenyClassify returns at most one match per rule, the first found, in
// rule order. It is a pure function of the text. A command it cannot follow
// to the end, nested too deep or too often, matches nothing.
func toolDenyClassify(command string) []toolDenyMatch {
	c := toolDenyScan{budget: 8*len(command) + 1024}
	c.shell(command, 0)
	if c.failed {
		return nil
	}
	var matches []toolDenyMatch
	for _, rule := range toolDenyRules {
		for _, m := range c.matches {
			if m.rule == rule {
				matches = append(matches, m)
			}
		}
	}
	return matches
}

type toolDenyScan struct {
	budget  int // bytes of shell text still allowed, nested commands included
	failed  bool
	matches []toolDenyMatch
}

func (c *toolDenyScan) add(rule, kind string, sum toolDenySum) {
	for _, m := range c.matches {
		if m.rule == rule {
			return
		}
	}
	c.matches = append(c.matches, toolDenyMatch{rule, kind, sum.done()})
}

// shell classifies every simple command in a piece of shell text.
func (c *toolDenyScan) shell(text string, depth int) {
	if c.failed {
		return
	}
	if c.budget -= len(text); c.budget < 0 {
		c.failed = true
		return
	}
	p := toolDenyParser{s: text}
	p.parse(0, 0, 0)
	if p.failed {
		c.failed = true
		return
	}
	for _, words := range p.cmds {
		c.command(words, depth)
	}
}

func (c *toolDenyScan) nested(text string, depth int) {
	if text != "" && depth < toolDenyMaxShells {
		c.shell(text, depth+1)
	}
}

func (c *toolDenyScan) command(words []string, depth int) {
	words = toolDenyCommandWords(words)
	if len(words) == 0 {
		return
	}
	name, args := toolDenyBase(words[0]), words[1:]
	switch name {
	case "pkill", "killall":
		c.pkill(name, args)
		c.deployer(name, name, "", args)
	case "kill":
		c.deployer(name, name, "", args)
	case "tmux":
		c.tmux(args)
	case "npm", "pnpm", "yarn", "bun":
		c.npm(name, args)
	case "node":
		c.node(args)
	case "deploy-static.mjs":
		sum := toolDenySum{}
		sum.add(name)
		sum.omitted += len(args)
		c.add(toolDenyOldSite, "node-script", sum)
	case "ssh":
		c.ssh(args, depth)
	case "bash", "sh", "zsh":
		for i := 0; i < len(args) && strings.HasPrefix(args[i], "-") || i < len(args) && strings.HasPrefix(args[i], "+"); i++ {
			a := args[i]
			if a == "-o" || a == "+o" || a == "-O" || a == "+O" {
				i++
				continue
			}
			if a[0] == '-' && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") {
				if i+1 < len(args) {
					c.nested(args[i+1], depth)
				}
				break
			}
		}
	case "eval":
		c.nested(strings.Join(args, " "), depth)
	}
}

// pkill fires host-pkill unless an option narrows the match to a user,
// parent, group, session, terminal or pid file, or only lists.
func (c *toolDenyScan) pkill(name string, args []string) {
	scoping, flags := "UuPgGstF", "fxinola"
	if name == "killall" {
		scoping, flags = "utls", "eIqvwm" // -l lists signals, -s only shows what it would do
	}
	sum := toolDenySum{}
	sum.add(name)
	for i, a := range args {
		if a == "--" {
			sum.omitted += len(args) - i
			break
		}
		if len(a) < 2 || a[0] != '-' || strings.HasPrefix(a, "--") {
			sum.omitted++
			continue
		}
		if sig, ok := toolDenySignal(a[1:]); ok {
			sum.add("-" + sig)
			continue
		}
		if strings.ContainsAny(a[1:], scoping) {
			return
		}
		sum.flags(a, flags)
	}
	c.add(toolDenyHostPkill, name, sum)
}

// deployer fires deployer-signal for a signalling command whose own words
// name the deployer's tmux session.
func (c *toolDenyScan) deployer(name, kind, sub string, args []string) {
	named := false
	for _, a := range args {
		if strings.Contains(a, toolDenyDeployerPrefix) {
			named = true
			break
		}
	}
	if !named {
		return
	}
	flags := ""
	switch name {
	case "pkill":
		flags = "fxinola"
	case "killall":
		flags = "eIqvwm"
	case "tmux":
		flags = "t"
	}
	sum := toolDenySum{}
	sum.add(name)
	skip := 0
	if sub != "" {
		sum.add(sub)
		// The words before the subcommand, and the word itself, are tmux's own.
		for skip < len(args) && toolDenyTmuxCommands[args[skip]] != sub {
			skip++
			sum.omitted++
		}
		skip++
	}
	named = false
	for i := skip; i < len(args); i++ {
		a := args[i]
		switch {
		case toolDenyDeployerName(a):
			sum.add(a)
			named = true
		case strings.Contains(a, toolDenyDeployerPrefix):
			if !named {
				sum.add(toolDenyDeployerPrefix)
				named = true
			}
			sum.omitted++
		case len(a) > 1 && a[0] == '-' && !strings.HasPrefix(a, "--"):
			if sig, ok := toolDenySignal(a[1:]); ok && name != "tmux" {
				sum.add("-" + sig)
			} else {
				sum.flags(a, flags)
			}
		default:
			sum.omitted++
		}
	}
	c.add(toolDenyDeployer, kind, sum)
}

func (c *toolDenyScan) tmux(args []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			if len(a) == 2 && strings.IndexByte("LSfcT", a[1]) >= 0 {
				i++
			}
			continue
		}
		if sub := toolDenyTmuxCommands[a]; sub != "" {
			c.deployer("tmux", "tmux-"+sub, sub, args)
		}
		return
	}
}

// npm fires old-site-deploy for `run deploy:static` or `run-script
// deploy:static`, with any options before either word.
func (c *toolDenyScan) npm(name string, args []string) {
	sum := toolDenySum{}
	sum.add(name)
	i := 0
	options := func() {
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			a := args[i]
			i++
			opt, _, attached := strings.Cut(a, "=")
			if opt == "--prefix" {
				sum.add(opt)
			} else {
				sum.omitted++
			}
			if !attached && toolDenyNpmValued[a] && i < len(args) {
				i++
				sum.omitted++
			}
		}
	}
	options()
	if i >= len(args) || args[i] != "run" && args[i] != "run-script" {
		return
	}
	sum.add(args[i])
	i++
	options()
	if i >= len(args) || args[i] != "deploy:static" {
		return
	}
	sum.add("deploy:static")
	sum.omitted += len(args) - i - 1
	c.add(toolDenyOldSite, "npm-run", sum)
}

// node fires old-site-deploy when the script it runs is deploy-static.mjs.
// A syntax check or an inline script does not run it.
func (c *toolDenyScan) node(args []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			switch a {
			case "--check", "-c", "-e", "--eval", "-p", "--print":
				return
			}
			if toolDenyNodeValued[a] {
				i++
			}
			continue
		}
		if toolDenyBase(a) != "deploy-static.mjs" {
			return
		}
		for _, later := range args[i+1:] {
			if later == "--check" || later == "-c" {
				return
			}
		}
		sum := toolDenySum{}
		sum.add("node")
		sum.add("deploy-static.mjs")
		sum.omitted += len(args) - 1
		c.add(toolDenyOldSite, "node-script", sum)
		return
	}
}

// ssh fires truenas-tailscale when the destination is the TrueNAS host and
// the remote command names tailscale. The remote command of any ssh is also
// classified as shell text of its own.
func (c *toolDenyScan) ssh(args []string, depth int) {
	sum := toolDenySum{}
	sum.add("ssh")
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[0] != '-' {
			break
		}
		if a == "--" {
			i++
			break
		}
		if len(a) == 2 && strings.IndexByte("opilt", a[1]) >= 0 {
			sum.add(a)
		} else {
			sum.omitted++
		}
		// The first letter that takes a value ends the option letters; the
		// value is the rest of the word or, when nothing is left, the next.
		if at := strings.IndexAny(a[1:], "bcDEeFIiJLlmOopQRSWwB"); at == len(a)-2 && i+1 < len(args) {
			i++
			sum.omitted++
		}
	}
	if i >= len(args) {
		return
	}
	host := args[i]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	remote := strings.Join(args[i+1:], " ")
	label := ""
	if strings.Contains(strings.ToLower(host), "truenas") {
		label = "truenas"
	}
	for _, addr := range toolDenyTrueNASAddrs {
		if host == addr {
			label = addr
		}
	}
	if label != "" && strings.Contains(strings.ToLower(remote), "tailscale") {
		sum.add(label)
		sum.add("tailscale")
		fields := strings.Fields(remote)
		sum.omitted += len(fields)
		for at, field := range fields {
			if strings.ToLower(toolDenyBase(field)) != "tailscale" {
				continue
			}
			sum.omitted--
			if at+1 < len(fields) && toolDenyTailscaleCommands[fields[at+1]] {
				sum.add(fields[at+1])
				sum.omitted--
			}
			break
		}
		c.add(toolDenyTrueNAS, "ssh", sum)
	}
	c.nested(remote, depth)
}

// toolDenySum builds a summary from words the classifier chose. Callers pass
// add a fixed word or a value checked against a fixed form; everything else
// is counted, not kept.
type toolDenySum struct {
	parts   []string
	omitted int
}

func (s *toolDenySum) add(part string) {
	if len(s.parts) >= toolDenyMaxParts {
		s.omitted++
		return
	}
	s.parts = append(s.parts, part)
}

// flags adds each letter of an option word when all are in the allowed set,
// and counts the word otherwise. An option's value is never a letter here.
func (s *toolDenySum) flags(word, allowed string) {
	for i := 1; i < len(word); i++ {
		if strings.IndexByte(allowed, word[i]) < 0 {
			s.omitted++
			return
		}
	}
	for i := 1; i < len(word); i++ {
		s.add("-" + word[i:i+1])
	}
}

func (s toolDenySum) done() []string {
	if s.omitted > 0 {
		return append(s.parts, fmt.Sprintf("(+%d args)", s.omitted))
	}
	return s.parts
}

// toolDenySignal reports a signal option's canonical text: a number from 1
// to 31, or a known name without its SIG prefix.
func toolDenySignal(s string) (string, bool) {
	if len(s) == 0 || len(s) > 9 {
		return "", false
	}
	if s[0] >= '0' && s[0] <= '9' {
		n := 0
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return "", false
			}
			n = n*10 + int(s[i]-'0')
		}
		if n < 1 || n > 31 {
			return "", false
		}
		return fmt.Sprint(n), true
	}
	name := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	return name, toolDenySignals[name]
}

// toolDenyDeployerName reports whether a whole word is a deployer session
// name: tt-deployer alone or with one suffix of lower-case letters and
// digits, at most 32.
func toolDenyDeployerName(word string) bool {
	rest, ok := strings.CutPrefix(word, toolDenyDeployerPrefix)
	if !ok || rest == "" {
		return ok
	}
	if rest[0] != '-' || len(rest) < 2 || len(rest) > 33 {
		return false
	}
	for i := 1; i < len(rest); i++ {
		if c := rest[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func toolDenyBase(word string) string {
	return word[strings.LastIndexByte(word, '/')+1:]
}

func toolDenyAssignment(word string) bool {
	for i := 0; i < len(word); i++ {
		c := word[i]
		if c == '=' {
			return i > 0
		}
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return false
}

// toolDenyCommandWords drops what stands before the command a simple
// command really runs: assignments, shell keywords and wrapper commands
// with their own options.
func toolDenyCommandWords(words []string) []string {
	for n := 0; n < 8; n++ {
		for len(words) > 0 && toolDenyAssignment(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			return nil
		}
		switch toolDenyBase(words[0]) {
		case "if", "then", "else", "elif", "do", "while", "until", "!", "nohup":
			words = words[1:]
		case "sudo":
			words = toolDenySkipOptions(words[1:], "ughpCDRTU")
		case "env":
			words = toolDenySkipOptions(words[1:], "uCSP")
		case "command":
			if len(words) > 1 && (strings.HasPrefix(words[1], "-v") || strings.HasPrefix(words[1], "-V")) {
				return nil // a lookup, nothing runs
			}
			words = toolDenySkipOptions(words[1:], "")
		case "exec":
			words = toolDenySkipOptions(words[1:], "a")
		case "time":
			words = toolDenySkipOptions(words[1:], "")
		case "timeout":
			if words = toolDenySkipOptions(words[1:], "sk"); len(words) > 0 {
				words = words[1:] // the duration
			}
		case "xargs":
			words = toolDenySkipOptions(words[1:], "InPLsEdaJRS")
		default:
			return words
		}
	}
	return words
}

// toolDenySkipOptions drops leading option words. valued holds the short
// options that take a value, attached or as the next word.
func toolDenySkipOptions(words []string, valued string) []string {
	for len(words) > 0 {
		w := words[0]
		if len(w) < 2 || w[0] != '-' {
			break
		}
		words = words[1:]
		if w == "--" {
			break
		}
		if strings.HasPrefix(w, "--") {
			if toolDenyWrapperValued[w] && len(words) > 0 {
				words = words[1:]
			}
			continue
		}
		if at := strings.IndexAny(w[1:], valued); valued != "" && at == len(w)-2 && len(words) > 0 {
			words = words[1:]
		}
	}
	return words
}

// toolDenyParser splits shell text into simple commands, each a list of
// words with the quoting removed. Text inside quotes is part of a word and
// never a command; a heredoc body is skipped. A substitution is kept, as
// written, in the word that holds it and its commands are listed too.
type toolDenyParser struct {
	s      string
	cmds   [][]string
	failed bool // substitutions nested too deep to follow
}

// toolDenySpecial marks the bytes that can end a run of plain word text.
var toolDenySpecial = func() (special [256]bool) {
	for _, c := range []byte("`)( \t\r\n;|&<>#{}\\'\"$") {
		special[c] = true
	}
	return
}()

type toolDenyHeredoc struct {
	end  string
	tabs bool // <<-: leading tabs are stripped from the terminator line
}

// parse reads from pos to the terminator of a substitution, ')' or '`', or
// to the end of the text when term is 0. It returns the terminator's index.
func (p *toolDenyParser) parse(pos int, term byte, nest int) int {
	s := p.s
	var (
		words    []string
		word     strings.Builder
		inWord   bool
		skipNext bool // the next word is a redirect's target
		heredocs []toolDenyHeredoc
		parens   int
	)
	endWord := func() {
		if !inWord {
			return
		}
		if skipNext {
			skipNext = false
		} else {
			words = append(words, word.String())
		}
		word.Reset()
		inWord = false
	}
	endCmd := func() {
		endWord()
		skipNext = false
		if len(words) > 0 {
			p.cmds = append(p.cmds, words)
			words = nil
		}
	}
	for pos < len(s) {
		c := s[pos]
		switch {
		case c == '`' && term == '`':
			endCmd()
			return pos
		case c == ')':
			if term == ')' && parens == 0 {
				endCmd()
				return pos
			}
			if parens > 0 {
				parens--
			}
			endCmd()
			pos++
		case c == '(':
			endCmd()
			parens++
			pos++
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
			pos++
		case c == '\n':
			endCmd()
			pos = toolDenySkipHeredocs(s, pos+1, heredocs)
			heredocs = nil
		case c == ';' || c == '|':
			endCmd()
			pos++
		case c == '&':
			if pos+1 < len(s) && s[pos+1] == '>' { // &> and &>> redirect
				endWord()
				for pos++; pos < len(s) && s[pos] == '>'; pos++ {
				}
				skipNext = true
			} else {
				endCmd()
				pos++
			}
		case c == '<' || c == '>':
			if inWord && toolDenyDigits(word.String()) {
				word.Reset() // the descriptor number of 2>file
				inWord = false
			} else {
				endWord()
			}
			if strings.HasPrefix(s[pos:], "<<") && !strings.HasPrefix(s[pos:], "<<<") {
				var doc toolDenyHeredoc
				if pos, doc = toolDenyHeredocWord(s, pos+2); doc.end != "" {
					heredocs = append(heredocs, doc)
				}
				continue
			}
			for pos++; pos < len(s) && strings.IndexByte("<>&|", s[pos]) >= 0; pos++ {
			}
			// <(command) and >(command) hold a command, not a file name.
			skipNext = pos >= len(s) || s[pos] != '('
		case c == '#' && !inWord:
			for pos < len(s) && s[pos] != '\n' {
				pos++
			}
		case c == '{' && !inWord && (pos+1 == len(s) || strings.IndexByte(" \t\n", s[pos+1]) >= 0):
			endCmd()
			pos++
		case c == '}' && !inWord:
			endCmd()
			pos++
		case c == '\\':
			if pos+1 < len(s) && s[pos+1] != '\n' {
				word.WriteByte(s[pos+1])
				inWord = true
			}
			pos += 2
		case c == '\'':
			end := strings.IndexByte(s[pos+1:], '\'')
			if end < 0 {
				end = len(s) - pos - 1
			}
			word.WriteString(s[pos+1 : pos+1+end])
			inWord = true
			pos += end + 2
		case c == '"':
			pos = p.quoted(pos+1, &word, nest)
			inWord = true
		case c == '$' || c == '`':
			pos = p.expansion(pos, &word, nest, false)
			inWord = true
		default:
			end := pos + 1
			for end < len(s) && !toolDenySpecial[s[end]] {
				end++
			}
			word.WriteString(s[pos:end])
			inWord = true
			pos = end
		}
	}
	endCmd()
	return len(s)
}

// quoted reads a double-quoted string and returns the index after it.
func (p *toolDenyParser) quoted(pos int, word *strings.Builder, nest int) int {
	s := p.s
	for pos < len(s) {
		switch c := s[pos]; c {
		case '"':
			return pos + 1
		case '\\':
			if pos+1 < len(s) {
				if next := s[pos+1]; strings.IndexByte("$`\"\\", next) >= 0 {
					word.WriteByte(next)
				} else if next != '\n' {
					word.WriteByte(c)
					word.WriteByte(next)
				}
			}
			pos += 2
		case '$', '`':
			pos = p.expansion(pos, word, nest, true)
		default:
			end := pos + 1
			for end < len(s) && strings.IndexByte("\"\\$`", s[end]) < 0 {
				end++
			}
			word.WriteString(s[pos:end])
			pos = end
		}
	}
	return len(s)
}

// expansion reads what starts with '$' or '`' and returns the index after
// it. A command substitution is written to the word as it stands, and its
// commands are parsed into the list.
func (p *toolDenyParser) expansion(pos int, word *strings.Builder, nest int, quoted bool) int {
	s := p.s
	end := pos + 1
	switch {
	case s[pos] == '`':
		end = p.substitution(pos+1, '`', nest) + 1
	case strings.HasPrefix(s[pos:], "$("):
		end = p.substitution(pos+2, ')', nest) + 1
	case strings.HasPrefix(s[pos:], "${"):
		if close := strings.IndexByte(s[pos:], '}'); close >= 0 {
			end = pos + close + 1
		} else {
			end = len(s)
		}
	case strings.HasPrefix(s[pos:], "$'") && !quoted:
		for end = pos + 2; end < len(s) && s[end] != '\''; end++ {
			if s[end] == '\\' {
				end++
			}
		}
		end++
	}
	if end > len(s) {
		end = len(s)
	}
	word.WriteString(s[pos:end])
	return end
}

func (p *toolDenyParser) substitution(pos int, term byte, nest int) int {
	if nest >= toolDenyMaxNest {
		p.failed = true
		return len(p.s)
	}
	return p.parse(pos, term, nest+1)
}

func toolDenyDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// toolDenyHeredocWord reads the terminator word after << and returns the
// index after it. Quotes around the word are dropped.
func toolDenyHeredocWord(s string, pos int) (int, toolDenyHeredoc) {
	var doc toolDenyHeredoc
	if pos < len(s) && s[pos] == '-' {
		doc.tabs = true
		pos++
	}
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t') {
		pos++
	}
	start := pos
	for pos < len(s) && strings.IndexByte(" \t\n;|&<>()", s[pos]) < 0 {
		pos++
	}
	doc.end = strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(s[start:pos])
	return pos, doc
}

// toolDenySkipHeredocs returns the index after the bodies of the heredocs
// opened on the line that just ended.
func toolDenySkipHeredocs(s string, pos int, heredocs []toolDenyHeredoc) int {
	for _, doc := range heredocs {
		for pos < len(s) {
			end := strings.IndexByte(s[pos:], '\n')
			line := s[pos:]
			next := len(s)
			if end >= 0 {
				line, next = s[pos:pos+end], pos+end+1
			}
			pos = next
			if doc.tabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == doc.end {
				break
			}
		}
	}
	return pos
}

// wouldDeny appends one would_deny row for each rule a Bash command
// matches. It has no result: a command it cannot read or classify, a panic
// or a deadline that has passed leaves the ledger as it was.
func (l toolLedger) wouldDeny(row toolLedgerRow, tool string, input json.RawMessage) {
	defer func() { _ = recover() }()
	// JSON writes one command byte as at most six, so a larger input cannot
	// hold a command within the cap and is not decoded.
	if tool != "Bash" || len(input) > 6*l.denyMax+4096 {
		return
	}
	var in struct {
		Command string `json:"command"`
	}
	// No partial classification: a cut inside a quote could invent a match.
	if json.Unmarshal(input, &in) != nil || in.Command == "" || len(in.Command) > l.denyMax {
		return
	}
	matches := toolDenyClassify(in.Command)
	if len(matches) == 0 {
		return
	}
	exact := toolRedactExact(l.token)
	rows := make([]toolLedgerRow, 0, len(matches))
	for _, m := range matches {
		row.Outcome, row.DenyRule, row.DenyKind = "would_deny", m.rule, m.kind
		row.DenySummary = toolDenySummary(m.parts, exact, l.started.Add(l.redactBudget))
		rows = append(rows, row)
	}
	if time.Since(l.started) >= l.deadline {
		return // the handler has already returned; write nothing late
	}
	l.append(rows...)
}

// toolDenySummary joins a match's parts and passes the text through the
// secret scan, replacing whatever it finds. A scan that did not finish
// gives no summary; the row still names its rule and kind.
func toolDenySummary(parts, exact []string, deadline time.Time) string {
	text := strings.Join(parts, " ")
	rewrite, counted, ok := redactScan(text, exact, deadline)
	if !ok {
		return ""
	}
	var out strings.Builder
	at := 0
	for _, span := range redactMerge(append(rewrite, counted...)) {
		out.WriteString(text[at:span.start])
		out.WriteString("[redacted]")
		at = span.end
	}
	out.WriteString(text[at:])
	return toolLedgerCutTo(out.String(), toolDenySummaryBytes)
}
