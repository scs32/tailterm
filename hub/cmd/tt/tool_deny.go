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
//
// The handler-gate report (feature wi_cbc08bd2b2a419a0) is report-only too
// and shares that check: the same parse of the same command is also matched
// against three rules for work the database handler owns, and each match
// appends one would_report row tagged with who ran it. It opens no file and
// takes the hub address from the TAILTERM_HUB environment value alone.

// Variables rather than constants so tests can change them. A hook call
// copies them once, before its goroutine starts.
var (
	toolDenyCheck      = toolLedger.wouldDeny
	toolDenyMaxCommand = 64 << 10 // a longer command is not classified at all
	toolGateOn         = true     // off, the handler-gate rules are not matched
	// Read once per classified command, so a test can make the gate panic.
	toolGateMatch = (*toolDenyScan).gateCommand
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

	toolGateWorkItem = "work-item-write"
	toolGateHubWrite = "hub-direct-write"
	toolGateHubDB    = "hub-db-direct"

	toolGateTagHelper  = "owner-helper"
	toolGateTagHandler = "handler"
	toolGateTagLead    = "lead-allowed"
	toolGateTagNone    = "none"

	toolGateHandlerPrefix = "db-handler"
	toolGateLeadPrefix    = "lead-"
	toolGateLeadForm      = "lead-form" // the kind of a write the item lead may make itself

	toolGateHubPort   = "18765"
	toolGateHubDir    = "/mnt/deepfreeze/tailterm-hub"
	toolGateTargetHub = "hub"        // the authority of TAILTERM_HUB
	toolGateTargetAny = "port-18765" // the hub's port on any other host
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
	deny, _ := toolGateClassify(command, "", false)
	return deny
}

// toolGateClassify parses the command once and returns the deny matches as
// toolDenyClassify does and, when gate is set, the handler-gate matches the
// same way: at most one per rule, in toolGateRules order. hub is the
// TAILTERM_HUB value. The gate matches never change the deny matches.
func toolGateClassify(command, hub string, gate bool) (deny, gates []toolDenyMatch) {
	c := toolDenyScan{budget: 8*len(command) + 1024, gate: gate}
	if gate {
		c.hubHost, c.hubPort = toolGateAuthority(hub, "")
	}
	c.shell(command, 0)
	if c.failed {
		return nil, nil
	}
	ordered := func(rules []string, found []toolDenyMatch) (matches []toolDenyMatch) {
		for _, rule := range rules {
			for _, m := range found {
				if m.rule == rule {
					matches = append(matches, m)
				}
			}
		}
		return
	}
	return ordered(toolDenyRules, c.matches), ordered(toolGateRules, c.gates)
}

type toolDenyScan struct {
	budget  int // bytes of shell text still allowed, nested commands included
	failed  bool
	matches []toolDenyMatch
	// The handler-gate report.
	gate             bool
	hubHost, hubPort string
	gates            []toolDenyMatch
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
	case "tt", "curl", "wget", "http", "https", "xh", "sqlite3":
		if c.gate {
			c.guarded(name, args)
		}
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
// A syntax check or an inline script, asked for before the file name, does
// not run it. What follows the name is the script's own and changes nothing.
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
// matches, and one would_report row for each handler-gate rule it matches,
// in one write. It has no result: a command it cannot read or classify, a panic
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
	matches, gates := toolGateClassify(in.Command, l.hub, l.gate)
	if len(matches)+len(gates) == 0 {
		return
	}
	exact := toolRedactExact(l.token)
	rows := make([]toolLedgerRow, 0, len(matches)+len(gates))
	call := row
	for _, m := range matches {
		row.Outcome, row.DenyRule, row.DenyKind = "would_deny", m.rule, m.kind
		row.DenySummary = toolDenySummary(m.parts, exact, l.started.Add(l.redactBudget))
		rows = append(rows, row)
	}
	rows = append(rows, l.gateRows(call, gates, exact)...)
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

// The handler-gate report: three rules for work the database handler owns.

// toolGateRules is the order gate matches are returned in.
var toolGateRules = []string{toolGateWorkItem, toolGateHubWrite, toolGateHubDB}

// toolGateWrites holds the tt subcommand words, as tt reads them by position,
// that change a work item, the Queue, the team queue or a verification
// record. Every other word in those positions is a read or no command.
var toolGateWrites = func() map[string]bool {
	writes := map[string]bool{}
	for prefix, subs := range map[string]string{
		"work-items":           "create update dispatch note evidence",
		"work-items scope":     "confirm save",
		"work-items narrative": "artifact-put link-put coverage-put report-put",
		"work-items triage":    "set confirm",
		"queue":                "action",
		"team queue":           "add policy limit provision scope rebind requeue fail accept integrated replace-lead remove reorder release abandon",
		"team queue wait":      "set clear",
		"verification":         "plan receipt",
	} {
		for _, sub := range strings.Fields(subs) {
			writes[prefix+" "+sub] = true
		}
	}
	return writes
}()

// toolGateOptions names the options of one client that take a value.
type toolGateOptions struct {
	short string
	long  map[string]bool
}

func toolGateLong(names string) map[string]bool {
	long := map[string]bool{}
	for _, name := range strings.Fields(names) {
		long["--"+name] = true
	}
	return long
}

var toolGateCurl = toolGateOptions{"AbcCdDeEFHKmoPQrtTuUwxXyYz", toolGateLong(`
	request data data-raw data-binary data-urlencode data-ascii json form form-string upload-file header user
	user-agent output output-dir cookie cookie-jar config connect-timeout max-time retry retry-delay retry-max-time
	proxy proxy-user proxy-header proxy-cacert proxy-cert proxy-key proxy-pass preproxy noproxy socks4 socks4a socks5
	socks5-hostname cacert capath cert cert-type key key-type pass pinnedpubkey crlfile ciphers tls-max tls13-ciphers
	resolve connect-to interface local-port dns-servers doh-url referer range write-out url oauth2-bearer aws-sigv4
	unix-socket abstract-unix-socket limit-rate rate max-redirs max-filesize dump-header trace trace-ascii stderr
	proto proto-default proto-redir keepalive-time netrc-file request-target expect100-timeout speed-limit speed-time
	time-cond quote continue-at alt-svc hsts etag-save etag-compare create-file-mode parallel-max delegation krb
	login-options service-name mail-from mail-rcpt mail-auth ftp-port ftp-account ftp-method pubkey engine
	happy-eyeballs-timeout-ms variable expand-url libcurl`)}

var toolGateWget = toolGateOptions{"OoPtTweUaiQlARDIXB", toolGateLong(`
	header post-data post-file body-data body-file method output-document output-file append-output user password
	user-agent directory-prefix tries timeout dns-timeout connect-timeout read-timeout wait waitretry execute
	input-file load-cookies save-cookies referer http-user http-password proxy-user proxy-password ca-certificate
	ca-directory certificate private-key bind-address limit-rate quota base level accept reject domains
	exclude-domains include-directories exclude-directories secure-protocol progress restrict-file-names
	max-redirect config`)}

// http, https and xh share HTTPie's options.
var toolGateHTTPie = toolGateOptions{"aAops", toolGateLong(`
	auth auth-type output session session-read-only proxy verify cert cert-key cert-key-pass timeout max-redirects
	max-headers print style pretty format-options response-charset response-mime raw boundary ssl ciphers
	default-scheme http-version unix-socket interface resolve`)}

var toolGateSqliteValued = map[string]bool{
	"cmd": true, "init": true, "separator": true, "newline": true, "nullvalue": true, "vfs": true,
	"maxsize": true, "mmap": true, "lookaside": true, "pagecache": true, "heap": true,
}

// The methods a summary may name. Any other is written as "method".
var toolGateMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
}

// walk hands every option to opt, with its value when the tables or an
// attached "=" give one, and every other word to pos. A value is never
// handed to pos, so a URL inside a header or a body is not a target.
func (o toolGateOptions) walk(args []string, opt func(name, value string), pos func(word string)) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, word := range args[i+1:] {
				pos(word)
			}
			return
		case strings.HasPrefix(a, "--"):
			name, value, attached := strings.Cut(a, "=")
			if !attached && o.long[name] && i+1 < len(args) {
				i++
				value = args[i]
			}
			opt(name, value)
		case len(a) > 1 && a[0] == '-':
			// The first letter that takes a value ends the letters: its value
			// is the rest of the word or, when nothing is left, the next word.
			for at := 1; at < len(a); at++ {
				if strings.IndexByte(o.short, a[at]) < 0 {
					opt(a[:1]+a[at:at+1], "")
					continue
				}
				value := a[at+1:]
				if value == "" && i+1 < len(args) {
					i++
					value = args[i]
				}
				opt(a[:1]+a[at:at+1], value)
				break
			}
		default:
			pos(a)
		}
	}
}

// guarded runs the gate rules on one command. A panic there ends the gate
// for this call and leaves the deny matches as they are.
func (c *toolDenyScan) guarded(name string, args []string) {
	defer func() {
		if recover() != nil {
			c.gate, c.gates = false, nil
		}
	}()
	toolGateMatch(c, name, args)
}

func (c *toolDenyScan) gateCommand(name string, args []string) {
	switch name {
	case "tt":
		c.tt(args)
	case "sqlite3":
		c.sqlite(args)
	default:
		c.http(name, args)
	}
}

// addGate keeps one match per gate rule: the first found, except that a
// write the item lead may make itself gives way to a later one it may not,
// so one allowed command cannot tag a whole call lead-allowed.
func (c *toolDenyScan) addGate(rule, kind string, sum toolDenySum) {
	for i, m := range c.gates {
		if m.rule == rule {
			if m.kind == toolGateLeadForm && kind != toolGateLeadForm {
				c.gates[i] = toolDenyMatch{rule, kind, sum.done()}
			}
			return
		}
	}
	c.gates = append(c.gates, toolDenyMatch{rule, kind, sum.done()})
}

// tt fires work-item-write when the leading words are a write subcommand.
func (c *toolDenyScan) tt(args []string) {
	words := 0
	for n := min(4, len(args)); n >= 2 && words == 0; n-- {
		if toolGateWrites[strings.Join(args[:n], " ")] {
			words = n
		}
	}
	if words == 0 {
		return
	}
	sum := toolDenySum{}
	sum.add("tt")
	for _, word := range args[:words] {
		sum.add(word)
	}
	kind, kept := "", words
	switch {
	case args[0] == "verification":
		kind = toolGateLeadForm
	case args[0] == "work-items" && args[1] == "update":
		// The done save with queue acceptance: --status done and all three
		// of --worktree, --branch and --commit. Only the option names are kept.
		done, seen, named := false, map[string]bool{}, 0
		for i := words; i < len(args); i++ {
			name, value, attached := strings.Cut(args[i], "=")
			switch name {
			case "--status":
				if !attached && i+1 < len(args) {
					i++
					value = args[i]
					named++
				}
				if done = value == "done"; done {
					named++
				}
			case "--worktree", "--branch", "--commit":
				if !seen[name] {
					named++
				}
				seen[name] = true
			}
		}
		if done && len(seen) == 3 {
			kind, kept = toolGateLeadForm, words+named
			for _, word := range []string{"--status", "done", "--worktree", "--branch", "--commit"} {
				sum.add(word)
			}
		}
	}
	sum.omitted += max(0, len(args)-kept)
	c.addGate(toolGateWorkItem, kind, sum)
}

// http fires hub-direct-write for a client whose own options make the
// request a write and one of whose URLs has the hub's authority.
func (c *toolDenyScan) http(name string, args []string) {
	var method, target string
	body, write := false, false
	scheme := ""
	if name == "https" {
		scheme = "https"
	}
	url := func(word string) {
		if target == "" {
			target = c.target(word, scheme)
		}
	}
	switch name {
	case "curl":
		get, head := false, false
		toolGateCurl.walk(args, func(opt, value string) {
			switch opt {
			case "-X", "--request":
				method = strings.ToUpper(value)
			case "-d", "--data", "--data-raw", "--data-binary", "--data-urlencode", "--json", "-F", "--form", "-T", "--upload-file":
				body = true
			case "-I", "--head":
				head = true
			case "-G", "--get":
				get = true
			case "--url":
				url(value)
			}
		}, url)
		// -G turns the data into a query, so the request stays a GET.
		write = method != "" && method != "GET" || (body || head) && !get
		if head && method == "" {
			method = "HEAD"
		}
	case "wget":
		toolGateWget.walk(args, func(opt, value string) {
			switch opt {
			case "--post-data", "--post-file", "--body-data", "--body-file":
				body = true
			case "--method":
				method = strings.ToUpper(value)
			}
		}, url)
		write = body || method != "" && method != "GET"
	default:
		var words []string
		toolGateHTTPie.walk(args, func(string, string) {}, func(word string) { words = append(words, word) })
		if len(words) > 1 && toolGateMethods[strings.ToUpper(words[0])] {
			method, words = strings.ToUpper(words[0]), words[1:]
		}
		if len(words) == 0 {
			return
		}
		// The first word is the URL; the rest are request items.
		url(words[0])
		for _, item := range words[1:] {
			body = body || toolGateBodyItem(item)
		}
		write = body || method != "" && method != "GET"
	}
	if !write || target == "" {
		return
	}
	sum := toolDenySum{}
	sum.add(name)
	switch {
	case method == "" || method == "GET":
	case toolGateMethods[method]:
		sum.add(method)
	default:
		sum.add("method")
	}
	if body {
		sum.add("data")
	}
	sum.add(target)
	sum.omitted += len(args) - 1 // every word but the target
	c.addGate(toolGateHubWrite, "", sum)
}

// toolGateBodyItem reports whether an HTTPie request item is a body field:
// its first separator is =, := or @. A header (name:value) and a query
// parameter (name==value) leave the request a GET.
func toolGateBodyItem(item string) bool {
	at := strings.IndexAny(item, ":=@")
	if at < 0 {
		return false
	}
	rest := item[at:]
	return rest[0] == '@' || strings.HasPrefix(rest, ":=") || rest[0] == '=' && !strings.HasPrefix(rest, "==")
}

// target judges one word by its URL authority alone and returns the fixed
// word for the hub it names, or nothing. A path, a query and a fragment are
// never read. The parser keeps a variable as written, so a word that starts
// with the TAILTERM_HUB variable has the hub in its authority position.
func (c *toolDenyScan) target(word, scheme string) string {
	for _, variable := range []string{"$TAILTERM_HUB", "${TAILTERM_HUB}"} {
		if rest, ok := strings.CutPrefix(word, variable); ok && (rest == "" || strings.IndexByte("/?#", rest[0]) >= 0) {
			return toolGateTargetHub
		}
	}
	host, port := toolGateAuthority(word, scheme)
	switch {
	case c.hubHost != "" && port == c.hubPort && strings.EqualFold(host, c.hubHost):
		return toolGateTargetHub
	case port == toolGateHubPort:
		return toolGateTargetAny
	}
	return ""
}

// toolGateAuthority returns the host and port of a URL's authority: what
// follows an optional scheme:// and an optional user:pass@, up to the first
// "/", "?" or "#". A missing port is the scheme's.
func toolGateAuthority(word, scheme string) (host, port string) {
	if at := strings.Index(word, "://"); at > 0 && toolGateScheme(word[:at]) {
		scheme, word = strings.ToLower(word[:at]), word[at+3:]
	}
	if end := strings.IndexAny(word, "/?#"); end >= 0 {
		word = word[:end]
	}
	word = word[strings.LastIndexByte(word, '@')+1:]
	host = word
	if end := strings.LastIndexByte(word, ']'); end >= 0 { // [address]:port
		host, port = word[:end+1], strings.TrimPrefix(word[end+1:], ":")
	} else if colon := strings.LastIndexByte(word, ':'); colon >= 0 {
		host, port = word[:colon], word[colon+1:]
	}
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	return host, port
}

func toolGateScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (i == 0 || (c < '0' || c > '9') && c != '+' && c != '-' && c != '.') {
			return false
		}
	}
	return s != ""
}

// sqlite fires hub-db-direct when the database operand, the first word that
// is neither an option nor an option's value, is the hub directory or under
// it. Later words are SQL text and are never judged.
func (c *toolDenyScan) sqlite(args []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			if toolGateSqliteValued[strings.TrimLeft(a, "-")] {
				i++
			}
			continue
		}
		a = strings.TrimPrefix(a, "file:")
		if query := strings.IndexByte(a, '?'); query >= 0 {
			a = a[:query]
		}
		if a == toolGateHubDir || strings.HasPrefix(a, toolGateHubDir+"/") {
			sum := toolDenySum{}
			sum.add("sqlite3")
			sum.add("hub-db")
			sum.omitted += len(args) - 1
			c.addGate(toolGateHubDB, "", sum)
		}
		return
	}
}

// gateTag names who ran a gate match, from what the hook already holds. The
// handler and the lead are told by name prefix and, for the lead, by the
// form of the command: the hook cannot ask the hub whose entry it names.
func (l toolLedger) gateTag(m toolDenyMatch) string {
	switch {
	case l.helper:
		return toolGateTagHelper
	case strings.HasPrefix(l.name, toolGateHandlerPrefix):
		return toolGateTagHandler
	case strings.HasPrefix(l.name, toolGateLeadPrefix) && m.kind == toolGateLeadForm:
		return toolGateTagLead
	}
	return toolGateTagNone
}

// toolGateName is the agent name a gate row may hold: cut to the stored
// length, and nothing unless every byte is a letter, a digit, "_", "-" or ".".
func toolGateName(name string) string {
	name = toolLedgerCut(name)
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return ""
		}
	}
	return name
}

// gateRows builds the would_report row of each gate match from the call's
// row. A panic here gives no gate rows and leaves the would-deny rows alone.
func (l toolLedger) gateRows(call toolLedgerRow, gates []toolDenyMatch, exact []string) (rows []toolLedgerRow) {
	defer func() {
		if recover() != nil {
			rows = nil
		}
	}()
	name := toolGateName(l.name)
	for _, m := range gates {
		row := call
		row.Outcome, row.ReportRule, row.ReportTag, row.AgentName = toolScopeOutcome, m.rule, l.gateTag(m), name
		row.ReportSummary = toolDenySummary(m.parts, exact, l.started.Add(l.redactBudget))
		rows = append(rows, row)
	}
	return rows
}
