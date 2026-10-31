package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The claim checker (feature wi_086dd63cab0abde0) is a warning, never a
// gate. Before tt send posts a RESULT or a lead's accept disposition, it
// compares the commits, verification commands and receipt files the message
// cites against the sender's own local tool-call ledger (tool_ledger.go) and
// prints what matched, what the ledger contradicts and what it does not hold.
// The message is posted exactly as it would have been: the check reads the
// envelope and never changes it, so the derived request id is the same.
//
// Only the sender's own ledger is read, read-only and bounded; nothing is
// written, and nothing reaches the hub, the Board or AIV. A sender with no
// ledger (a Codex worker, no agent identity) is not checked and sees nothing.
// Counts are not checked: the ledger keeps a digest of the output, never the
// output. See the claim checker page in docs/, "Claim checker for Claude
// sessions".

// claimCheckBudget bounds the whole check. A variable so tests can shrink it.
var claimCheckBudget = 500 * time.Millisecond

// claimCheckWrappers is how many host-lock exec wrappers a receipt's run is
// followed through, as the evidence classifier does.
const claimCheckWrappers = 2

type claimState int

const (
	claimContradicted claimState = iota
	claimUnproven
	claimUnmatched
	claimMatched
)

var claimStateNames = map[claimState]string{
	claimContradicted: "contradicted", claimUnproven: "unproven", claimUnmatched: "unmatched", claimMatched: "matched",
}

type claimKind int

const (
	claimCommit claimKind = iota
	claimCommand
	claimReceipt
)

// citedClaim is one checkable claim the message makes.
type citedClaim struct {
	kind    claimKind
	source  string   // where the message cites it, e.g. "ref commit" or "e1"
	text    string   // the commit, the command or the receipt path
	outcome string   // a command's claimed outcome
	words   []string // a command's single simple command
}

type claimFinding struct {
	claim  citedClaim
	state  claimState
	detail string
}

// claimReport is what one check found. notChecked counts citations that
// cannot be checked locally, with their reasons in first-seen order.
type claimReport struct {
	findings   []claimFinding
	notChecked int
	reasons    []string
	skipped    bool // the budget ran out
}

func (r *claimReport) skip(reason string) {
	r.notChecked++
	if !slices.Contains(r.reasons, reason) {
		r.reasons = append(r.reasons, reason)
	}
}

const (
	claimReasonCommand = "commands that are not one isolated verification command"
	claimReasonReceipt = "receipts without a readable local file"
)

// claimCheckable reports whether a message of this kind is checked: every
// RESULT, and a notice that is a lead's accept disposition.
func claimCheckable(m api.Envelope) bool {
	switch m.Kind {
	case "result":
		return true
	case "notice":
		return m.Review != nil && m.Review.Mode == "disposition" && m.Review.Disposition == "accept"
	}
	return false
}

var claimHex = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// claimCommitToken reports whether a value is one commit-shaped hex token.
// A run of digits alone is a message or receipt number far more often than
// an abbreviated commit, so it is not taken as one.
func claimCommitToken(v string) bool {
	return claimHex.MatchString(v) && strings.Trim(v, "0123456789") != ""
}

// claimExtract lists the checkable claims of a message, reading only its
// structured fields. Free text is never parsed.
func claimExtract(m api.Envelope, r *claimReport) []citedClaim {
	var claims []citedClaim
	seen := map[string]bool{}
	commit := func(v, source string) {
		v = strings.TrimSpace(v)
		if !claimCommitToken(v) || seen[strings.ToLower(v)] {
			return
		}
		seen[strings.ToLower(v)] = true
		claims = append(claims, citedClaim{kind: claimCommit, source: source, text: strings.ToLower(v)})
	}
	refKeys := claimSortedKeys(m.Refs)
	for _, k := range refKeys {
		commit(m.Refs[k], "ref "+k)
	}
	evKeys := claimSortedKeys(m.Evidence)
	for _, k := range evKeys {
		if ev := m.Evidence[k]; ev.Type == "commit" {
			commit(ev.Value, k)
		}
	}
	commit(m.Body.Candidate, "candidate")
	if m.Review != nil {
		commit(m.Review.Candidate, "review candidate")
	}
	for _, k := range evKeys {
		ev := m.Evidence[k]
		if ev.Type != "command" {
			continue
		}
		words, ok := claimSingleCommand(ev.Value)
		if !ok || toolEvidenceKinds(ev.Value, toolEvidenceCommandBytes) == nil {
			r.skip(claimReasonCommand)
			continue
		}
		claims = append(claims, citedClaim{kind: claimCommand, source: k, text: ev.Value, outcome: ev.Outcome, words: words})
	}
	receipt := func(v, source string) {
		v = strings.TrimSpace(v)
		base := filepath.Base(v)
		if !filepath.IsAbs(v) || (base != "receipt.json" && base != "targeted-receipt.json") {
			r.skip(claimReasonReceipt)
			return
		}
		if info, err := os.Stat(v); err != nil || !info.Mode().IsRegular() {
			r.skip(claimReasonReceipt)
			return
		}
		claims = append(claims, citedClaim{kind: claimReceipt, source: source, text: filepath.Clean(v)})
	}
	for _, k := range refKeys {
		if strings.Contains(strings.ToLower(k), "receipt") {
			receipt(m.Refs[k], "ref "+k)
		}
	}
	for _, a := range m.Attachments {
		// Only an attachment named like a receipt is a receipt citation.
		if base := filepath.Base(strings.TrimSpace(a)); base == "receipt.json" || base == "targeted-receipt.json" {
			receipt(a, "attachment")
		}
	}
	return claims
}

func claimSortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// claimIsolated is the syntax guard both a cited command and a ledger row
// must pass before either is parsed: read left to right with single and
// double quotes tracked, it has outside quotes none of ; & | ( ) < > ` $ # { }
// a newline, a carriage return or a backslash; inside double quotes no $, `
// or backslash; and every quote closed. Text that passes is one command whose
// exit code is its own: no list, pipe, substitution, redirect, background
// start, comment or group can hide in it. Quoted metacharacters stay
// allowed, so -run 'A|B' is fine.
func claimIsolated(s string) bool {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch quote {
		case '\'':
			if c == '\'' {
				quote = 0
			}
		case '"':
			switch c {
			case '"':
				quote = 0
			case '$', '`', '\\':
				return false
			}
		default:
			switch c {
			case '\'', '"':
				quote = c
			case ';', '&', '|', '(', ')', '<', '>', '`', '$', '#', '{', '}', '\n', '\r', '\\':
				return false
			}
		}
	}
	return quote == 0
}

// claimSingleCommand returns the words of text that passes the isolation
// guard and that the would-deny parser reads as exactly one simple command.
func claimSingleCommand(text string) ([]string, bool) {
	if text == "" || len(text) > toolEvidenceCommandBytes || !claimIsolated(text) {
		return nil, false
	}
	if strings.HasSuffix(strings.TrimRight(text, " \t"), "&") {
		return nil, false
	}
	p := toolDenyParser{s: text}
	p.parse(0, 0, 0)
	if p.failed || len(p.cmds) != 1 || len(p.cmds[0]) == 0 {
		return nil, false
	}
	return p.cmds[0], true
}

// claimRow is one evidence row of the sender's ledger, read once.
type claimRow struct {
	at       time.Time
	order    int
	ev       *toolEvidence
	words    []string // its single simple command, when it is one
	isolated bool
}

// claimLedger reads the evidence rows of the agent's own ledger, the rotated
// file first. ok is false when the agent has no ledger: no identity, an id
// the ledger refuses, no ledger directory, or neither file present. Nothing
// is written. Past the deadline it stops and reports expired.
func claimLedger(agent string, deadline time.Time) (rows []claimRow, ok, expired bool) {
	root := toolLedgerRoot()
	if root == "" || !toolLedgerAgentOK(agent) {
		return nil, false, false
	}
	marker := []byte(`"evidence"`)
	// Rows repeat the same few commands, so each text is parsed once.
	type parsed struct {
		words []string
		ok    bool
	}
	cache := map[string]parsed{}
	for _, name := range []string{"ledger.jsonl.1", "ledger.jsonl"} {
		if time.Now().After(deadline) {
			return rows, ok, true
		}
		f, err := os.Open(filepath.Join(root, agent, name))
		if err != nil {
			continue
		}
		ok = true
		data, err := io.ReadAll(io.LimitReader(f, 4*toolLedgerMaxBytes))
		_ = f.Close()
		if err != nil {
			continue
		}
		for n, line := range bytes.Split(data, []byte("\n")) {
			if n%256 == 0 && time.Now().After(deadline) {
				return rows, ok, true
			}
			if len(line) == 0 || !bytes.Contains(line, marker) {
				continue
			}
			// Only the fields the check reads are decoded.
			var row struct {
				Time     string        `json:"time"`
				Agent    string        `json:"agent"`
				Evidence *toolEvidence `json:"evidence"`
			}
			if json.Unmarshal(line, &row) != nil || row.Agent != agent || row.Evidence == nil {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, row.Time)
			p, seen := cache[row.Evidence.Command]
			if !seen {
				p.words, p.ok = claimSingleCommand(row.Evidence.Command)
				cache[row.Evidence.Command] = p
			}
			rows = append(rows, claimRow{at: at, order: len(rows), ev: row.Evidence, words: p.words, isolated: p.ok})
		}
	}
	return rows, ok, false
}

// claimLatest picks the latest of the matching rows, by time and then by
// position in the ledger.
func claimLatest(rows []claimRow, match func(claimRow) bool) (claimRow, bool) {
	var best claimRow
	found := false
	for _, r := range rows {
		if !match(r) {
			continue
		}
		if !found || r.at.After(best.at) || (!r.at.Before(best.at) && r.order > best.order) {
			best, found = r, true
		}
	}
	return best, found
}

var (
	claimPassWords = regexp.MustCompile(`(?i)\b(ok|pass|passed|green|clean)\b|\bexit 0\b`)
	claimFailWords = regexp.MustCompile(`(?i)fail|error|panic|\bexit [1-9]`)
	// TAP's "not ok" is a failure, never the "ok" inside it.
	claimNotOK = regexp.MustCompile(`(?i)\bnot ok\b`)
	// A zero count of failures or errors says the run passed.
	claimZeroFailures = regexp.MustCompile(`(?i)\b(0|no)\s+(failed|fail|failures|failure|errors|error)\b`)
)

// claimOutcome reads a claimed outcome as a pass (1), a failure (-1) or
// neither (0). "not ok" is read as a failure first; then a zero count of
// failures or errors ("0 failed", "no errors") is read as a pass, so
// "62 passed, 0 failed" and "no errors" are passes. Text that still names
// both, such as "1 passed, 3 failed", is neither.
func claimOutcome(text string) int {
	text = claimNotOK.ReplaceAllString(text, " fail ")
	text = claimZeroFailures.ReplaceAllString(text, " pass ")
	pass, fail := claimPassWords.MatchString(text), claimFailWords.MatchString(text)
	switch {
	case pass && !fail:
		return 1
	case fail && !pass:
		return -1
	}
	return 0
}

func claimShort(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func claimAt(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.UTC().Format(time.RFC3339)
}

// claimCheckCommit: some evidence row ran at a commit the cited one prefixes.
func claimCheckCommit(c citedClaim, rows []claimRow) claimFinding {
	noCommit := 0
	for _, r := range rows {
		if r.ev.Commit == "" {
			noCommit++
			continue
		}
		if strings.HasPrefix(strings.ToLower(r.ev.Commit), c.text) {
			return claimFinding{claim: c, state: claimMatched}
		}
	}
	detail := "no verification run in your ledger at this commit"
	if noCommit > 0 {
		detail += fmt.Sprintf("; %d of your runs recorded no commit (cd before the command, or outside a repository)", noCommit)
	}
	return claimFinding{claim: c, state: claimUnmatched, detail: detail}
}

// claimCheckCommand: only an identical, isolated single command is credited,
// and the latest such run decides.
func claimCheckCommand(c citedClaim, rows []claimRow, single string) claimFinding {
	latest, ok := claimLatest(rows, func(r claimRow) bool { return r.isolated && slices.Equal(r.words, c.words) })
	if !ok {
		return claimFinding{claim: c, state: claimUnmatched, detail: "no identical single command in your ledger"}
	}
	exit, outcome := latest.ev.ExitCode, claimOutcome(c.outcome)
	switch {
	case exit != nil && outcome > 0 && *exit != 0:
		return claimFinding{claim: c, state: claimContradicted, detail: fmt.Sprintf("your latest run exited %d at %s", *exit, claimAt(latest.at))}
	case exit != nil && outcome < 0 && *exit == 0:
		return claimFinding{claim: c, state: claimContradicted, detail: "your latest run exited 0 at " + claimAt(latest.at)}
	}
	if single != "" && latest.ev.Commit != "" && !strings.HasPrefix(strings.ToLower(latest.ev.Commit), single) {
		return claimFinding{claim: c, state: claimContradicted, detail: fmt.Sprintf("ran at %s, not %s", claimShort(latest.ev.Commit), single)}
	}
	if exit == nil {
		return claimFinding{claim: c, state: claimUnproven, detail: "exit not recorded"}
	}
	// An outcome read neither way is never credited by a failed run.
	if outcome == 0 && *exit != 0 {
		return claimFinding{claim: c, state: claimUnproven, detail: fmt.Sprintf("the outcome reads as neither a pass nor a failure and your latest run exited %d at %s", *exit, claimAt(latest.at))}
	}
	return claimFinding{claim: c, state: claimMatched, detail: fmt.Sprintf("exit %d at %s", *exit, claimAt(latest.at))}
}

// claimReceiptFile is the part of a verify-matrix receipt the check reads.
type claimReceiptFile struct {
	Commit      string                     `json:"commit"`
	Environment map[string]json.RawMessage `json:"environment"`
	Checks      []struct {
		ExitCode     *int `json:"exitCode"`
		KnownFailure bool `json:"knownFailure"`
	} `json:"checks"`
}

// eligible is receiptEligible in scripts/verify-matrix.mjs: no home cleanup
// error, and every check exited 0 or is a known failure.
func (f claimReceiptFile) eligible() bool {
	switch v := strings.TrimSpace(string(f.Environment["VERIFICATION_HOME_CLEANUP_ERROR"])); v {
	case "", "null", "false", "0", `""`:
	default:
		return false
	}
	for _, c := range f.Checks {
		if (c.ExitCode == nil || *c.ExitCode != 0) && !c.KnownFailure {
			return false
		}
	}
	return true
}

// claimReadReceipt reads a cited receipt, never past the hub's limit for a
// verification body. A file it cannot read or decode is not checkable.
func claimReadReceipt(path string) (claimReceiptFile, bool) {
	var out claimReceiptFile
	f, err := os.Open(path)
	if err != nil {
		return out, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, api.MaxVerificationBody))
	if err != nil || json.Unmarshal(data, &out) != nil {
		return out, false
	}
	return out, true
}

// claimMatrixRun reads a row's single command as a verify-matrix run:
// node [options] .../verify-matrix.mjs MODE INPUT OUTPUT [flags], with
// assignments and wrapper commands dropped and up to two host-lock exec
// wrappers followed. It returns the mode and the output directory.
func claimMatrixRun(words []string, wrapped int) (mode, output string, ok bool) {
	words = toolDenyCommandWords(words)
	if len(words) == 0 || toolDenyBase(words[0]) != "node" {
		return "", "", false
	}
	args := words[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			switch a {
			case "--check", "-c", "-e", "--eval", "-p", "--print", "--test":
				return "", "", false
			}
			if toolDenyNodeValued[a] {
				i++
			}
			continue
		}
		switch toolDenyBase(a) {
		case "verify-matrix.mjs":
			if i+3 >= len(args) {
				return "", "", false
			}
			return args[i+1], args[i+3], true
		case "verify-matrix-host-lock.mjs":
			if i+1 >= len(args) || args[i+1] != "exec" || wrapped >= claimCheckWrappers {
				return "", "", false
			}
			for j := i + 2; j < len(args); j++ {
				if args[j] == "--" {
					return claimMatrixRun(args[j+1:], wrapped+1)
				}
			}
		}
		return "", "", false
	}
	return "", "", false
}

// claimCheckReceipt: the receipt must have been written by a run or targeted
// matrix run in the ledger whose absolute output directory is the receipt's
// own directory, exactly; the latest such run decides.
func claimCheckReceipt(c citedClaim, rows []claimRow, single string, r *claimReport) (claimFinding, bool) {
	file, ok := claimReadReceipt(c.text)
	if !ok {
		r.skip(claimReasonReceipt)
		return claimFinding{}, false
	}
	wantMode := "run"
	if filepath.Base(c.text) == "targeted-receipt.json" {
		wantMode = "targeted"
	}
	dir := filepath.Dir(c.text)
	latest, found := claimLatest(rows, func(row claimRow) bool {
		if !row.isolated {
			return false
		}
		mode, output, ok := claimMatrixRun(row.words, 0)
		return ok && mode == wantMode && filepath.IsAbs(output) && filepath.Clean(output) == dir
	})
	if !found {
		return claimFinding{claim: c, state: claimUnmatched, detail: "no matrix run in your ledger wrote this receipt"}, true
	}
	exit, eligible := latest.ev.ExitCode, file.eligible()
	receiptCommit := strings.ToLower(file.Commit)
	switch {
	case exit != nil && *exit == 0 && !eligible:
		return claimFinding{claim: c, state: claimContradicted, detail: "your run exited 0 but the receipt is not eligible"}, true
	case exit != nil && *exit != 0 && eligible:
		return claimFinding{claim: c, state: claimContradicted, detail: fmt.Sprintf("the receipt is eligible but your run exited %d", *exit)}, true
	case latest.ev.Commit != "" && receiptCommit != "" &&
		!strings.HasPrefix(strings.ToLower(latest.ev.Commit), receiptCommit) && !strings.HasPrefix(receiptCommit, strings.ToLower(latest.ev.Commit)):
		return claimFinding{claim: c, state: claimContradicted, detail: fmt.Sprintf("your run was at %s, the receipt names %s", claimShort(latest.ev.Commit), claimShort(receiptCommit))}, true
	case single != "" && receiptCommit != "" && !strings.HasPrefix(receiptCommit, single):
		return claimFinding{claim: c, state: claimContradicted, detail: fmt.Sprintf("the receipt names %s, not %s", claimShort(receiptCommit), single)}, true
	case exit == nil:
		return claimFinding{claim: c, state: claimUnproven, detail: "exit not recorded"}, true
	}
	return claimFinding{claim: c, state: claimMatched}, true
}

// claimRun is the check without its budget. ok is false when the agent has
// no ledger.
func claimRun(agent string, m api.Envelope, deadline time.Time) (claimReport, bool) {
	var r claimReport
	rows, ok, expired := claimLedger(agent, deadline)
	if !ok {
		return r, false
	}
	if expired {
		return claimReport{skipped: true}, true
	}
	claims := claimExtract(m, &r)
	single := ""
	commits := 0
	for _, c := range claims {
		if c.kind == claimCommit {
			commits++
			single = c.text
		}
	}
	if commits != 1 {
		single = ""
	}
	for _, c := range claims {
		switch c.kind {
		case claimCommit:
			r.findings = append(r.findings, claimCheckCommit(c, rows))
		case claimCommand:
			r.findings = append(r.findings, claimCheckCommand(c, rows, single))
		case claimReceipt:
			if f, ok := claimCheckReceipt(c, rows, single, &r); ok {
				r.findings = append(r.findings, f)
			}
		}
	}
	if time.Now().After(deadline) {
		return claimReport{skipped: true}, true
	}
	return r, true
}

// claimHasLedger reports whether the agent has a ledger file on this host.
// It runs before the budget starts, so a sender with no ledger is never told
// its check was skipped.
func claimHasLedger(agent string) bool {
	root := toolLedgerRoot()
	if root == "" || !toolLedgerAgentOK(agent) {
		return false
	}
	for _, name := range []string{"ledger.jsonl.1", "ledger.jsonl"} {
		if info, err := os.Stat(filepath.Join(root, agent, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// claimCheck runs the check for the agent's message within the budget. ok is
// false when nothing is checked: a kind that is not checked, or no ledger.
func claimCheck(agent string, m api.Envelope) (claimReport, bool) {
	if !claimCheckable(m) || !claimHasLedger(agent) {
		return claimReport{}, false
	}
	budget := claimCheckBudget
	type result struct {
		r  claimReport
		ok bool
	}
	done := make(chan result, 1)
	deadline := time.Now().Add(budget)
	go func() {
		r, ok := claimRun(agent, m, deadline)
		done <- result{r, ok}
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.r, res.ok
	case <-timer.C:
		return claimReport{skipped: true}, true
	}
}

// failed reports whether any claim is not matched, or the check timed out.
func (r claimReport) failed() bool {
	if r.skipped {
		return true
	}
	for _, f := range r.findings {
		if f.state != claimMatched {
			return true
		}
	}
	return false
}

func claimBudgetText() string {
	if claimCheckBudget%time.Millisecond == 0 {
		return fmt.Sprintf("%d ms", claimCheckBudget.Milliseconds())
	}
	return claimCheckBudget.String()
}

func (c citedClaim) label() string {
	switch c.kind {
	case claimCommit:
		return fmt.Sprintf("commit %s (%s)", c.text, c.source)
	case claimCommand:
		text := c.source + " " + c.text
		if c.outcome != "" {
			text += " -> " + c.outcome
		}
		return text
	}
	return fmt.Sprintf("receipt %s (%s)", c.text, c.source)
}

// writeClaimReport prints a report: nothing when the message cites nothing
// checkable, one line when every checkable claim matched, else every claim.
// dryRun names the --claims-only mode in the heading.
func writeClaimReport(w io.Writer, r claimReport, dryRun bool) {
	if r.skipped {
		fmt.Fprintf(w, "claim check skipped: over %s\n", claimBudgetText())
		return
	}
	if len(r.findings) == 0 {
		// Silent on a send; the dry run says what it found.
		if dryRun && r.notChecked > 0 {
			fmt.Fprintf(w, "claim check: no checkable claims; %d not checked (%s)\n", r.notChecked, strings.Join(r.reasons, ", "))
		} else if dryRun {
			fmt.Fprintln(w, "claim check: no checkable claims")
		}
		return
	}
	if !r.failed() {
		line := fmt.Sprintf("claim check: %d cited claims match your tool ledger", len(r.findings))
		if len(r.findings) == 1 {
			line = "claim check: 1 cited claim matches your tool ledger"
		}
		if r.notChecked > 0 {
			line += fmt.Sprintf("; %d not checked (%s)", r.notChecked, strings.Join(r.reasons, ", "))
		}
		fmt.Fprintln(w, line)
		return
	}
	heading := "warning only, the message is sent unchanged"
	if dryRun {
		heading = "dry run, nothing is sent"
	}
	fmt.Fprintf(w, "claim check (your local tool ledger; %s):\n", heading)
	findings := slices.Clone(r.findings)
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].state < findings[j].state })
	for _, f := range findings {
		line := fmt.Sprintf("  %-13s %s", claimStateNames[f.state], f.claim.label())
		if f.detail != "" {
			line += ": " + f.detail
		}
		fmt.Fprintln(w, line)
	}
	if r.notChecked > 0 {
		fmt.Fprintf(w, "  %-13s %d (%s)\n", "not checked", r.notChecked, strings.Join(r.reasons, ", "))
	}
}
