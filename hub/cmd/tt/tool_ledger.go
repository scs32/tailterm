package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// The tool-call ledger (feature wi_f38d51348f280538) is observe-only. Claude
// Code runs `tt hook tool` on PreToolUse, PostToolUse and PostToolUseFailure;
// each tool call leaves one private local row. The hook never writes to
// stderr, never talks to the hub, and cannot make tt exit non-zero, so it
// cannot deny or delay a call. See docs/claude-wake.md.
//
// It writes to stdout in one case only: secret redaction (tool_redact.go),
// which is off unless the host's relay.json says otherwise, answers a
// PostToolUse with the same output and its token-shaped values replaced.
//
// The would-deny report (tool_deny.go) is report-only: a PreToolUse whose
// Bash command matches a protected-operation rule leaves one extra row per
// rule. It prints nothing and denies nothing.
//
// The owned-paths report (tool_scope.go) is report-only too: a PreToolUse of
// an edit tool whose file is outside the paths of the agent's latest ASSIGN
// leaves one extra row. It prints nothing and denies nothing.
//
// The handler-gate report (tool_deny.go) is report-only as well: a PreToolUse
// whose Bash command is a work-item write through tt, a non-GET call to the
// hub or direct sqlite3 access to the hub database leaves one extra row per
// rule, tagged with who ran it. It prints nothing and denies nothing.
//
// Verification evidence (feature wi_95d33b4e3248bac5) is local evidence
// only. A Bash call that runs a recognised verification command (go test,
// go vet, npm test, node --test, verify-matrix.mjs) gets one more field on
// its own row: the command text with secrets replaced, the exit code Claude
// Code reports, a SHA-256 of the output and the commit HEAD named when the
// call started. The output, the cwd and the repository path are never
// stored, and no process is started. See docs/claude-mods-evidence-capture.md.
//
// The owner helper's session has no agent identity in its environment. Its
// calls are ledgered under the helper registered from that exact runtime
// session, read from the host helper file (bug wi_b3e8ecd5a2ee01b3).

// hookHandlers holds the tt hook names that cmdHook hands over before it
// reads stdin. A handler has no return value, so it cannot fail the runtime.
var hookHandlers = map[string]func(e env, args []string){}

func init() { hookHandlers["tool"] = toolLedgerHook }

// Variables rather than constants so tests can change them. A hook call
// copies them once, before its goroutine starts.
var (
	toolLedgerDeadline         = 150 * time.Millisecond // the handler's whole wait
	toolLedgerMaxInput   int64 = 8 << 20                // stdin bytes read
	toolLedgerMaxBytes   int64 = 4 << 20                // ledger.jsonl size that rotates
	toolLedgerMaxPending       = 256                    // pending files per agent
	toolLedgerPendingAge       = 24 * time.Hour
	toolLedgerLockWait         = 50 * time.Millisecond
	toolLedgerNow              = time.Now
	toolLedgerWrite            = func(f *os.File, b []byte) error { _, err := f.Write(b); return err }
	toolLedgerRemove           = os.Remove
)

const (
	toolLedgerEvictBatch   = 32  // unknown rows one pre may write
	toolLedgerPendingBytes = 512 // largest pending file
	toolLedgerFieldBytes   = 128 // largest stored name or id
	toolLedgerLockRetry    = 2 * time.Millisecond
	toolLedgerClaimGrace   = time.Minute // an eviction claim older than this has no live hook
)

// toolLedger is one hook call's copy of the settings and its agent directory.
type toolLedger struct {
	dir        string
	task, run  string // identity written on every row, with the agent
	agent      string
	started    time.Time
	deadline   time.Duration
	maxInput   int64
	maxBytes   int64
	maxPending int
	pendingAge time.Duration
	lockWait   time.Duration
	now        func() time.Time
	write      func(*os.File, []byte) error
	remove     func(string) error
	// Secret redaction (tool_redact.go). The token is compared against tool
	// output in memory and never written.
	token        string
	redactMode   string
	redactBudget time.Duration
	redactMax    int
	stdout       io.Writer
	// The would-deny report (tool_deny.go), run after a pre is recorded.
	deny    func(toolLedger, toolLedgerRow, string, json.RawMessage)
	denyMax int
	// The owned-paths report (tool_scope.go), run after the would-deny report.
	scope func(toolLedger, toolLedgerRow, string, json.RawMessage, string)
	// The handler-gate report (tool_deny.go), part of the would-deny check.
	// hub is the TAILTERM_HUB environment value alone, never the configured
	// address; it is compared against the command and never written.
	gate   bool
	name   string // the agent's name, or the helper file's
	helper bool   // the identity came from the owner helper's file
	hub    string
	// Verification evidence: the output is not decoded after this, from the start.
	evidenceBudget time.Duration
}

// toolHookInput holds only the fields the ledger uses. tool_response, error
// and transcript_path are skipped by the decoder and never kept. cwd is
// kept raw, so a cwd of the wrong type cannot fail the decode, read as a
// string for the owned-paths report and the evidence commit and never
// stored. Secret redaction decodes tool_response itself, and only when it is
// switched on; verification evidence decodes tool_response or error itself,
// only for a verification command's post, and stores only their digest.
type toolHookInput struct {
	Event       string          `json:"hook_event_name"`
	Session     string          `json:"session_id"`
	Tool        string          `json:"tool_name"`
	ToolUseID   string          `json:"tool_use_id"`
	Input       json.RawMessage `json:"tool_input"`
	DurationMS  *float64        `json:"duration_ms"`
	IsInterrupt bool            `json:"is_interrupt"`
	Cwd         json.RawMessage `json:"cwd"`
}

type toolLedgerRow struct {
	V              int    `json:"v"`
	Time           string `json:"time"`
	Task           string `json:"task"`
	Agent          string `json:"agent"`
	Run            string `json:"run"`
	Session        string `json:"session"`
	Tool           string `json:"tool"`
	ToolUseID      string `json:"toolUseId"`
	ArgsDigest     string `json:"argsDigest"`
	Outcome        string `json:"outcome"`
	DurationMs     *int64 `json:"durationMs,omitempty"`
	DurationSource string `json:"durationSource,omitempty"`
	Oversize       bool   `json:"oversize,omitempty"`
	// Absent unless secret redaction is switched on for the host.
	RedactMode  string         `json:"redactMode,omitempty"`
	RedactCount *int           `json:"redactCount,omitempty"`
	RedactKinds map[string]int `json:"redactKinds,omitempty"`
	RedactSkip  string         `json:"redactSkip,omitempty"`
	// Only on a would_deny row (tool_deny.go).
	DenyRule    string `json:"denyRule,omitempty"`
	DenyKind    string `json:"denyKind,omitempty"`
	DenySummary string `json:"denySummary,omitempty"`
	// Only on a would_report row: the rule, then the path and ASSIGN of an
	// owned-paths row (tool_scope.go) or the tag, summary and agent name of a
	// handler-gate row (tool_deny.go).
	ReportRule    string `json:"reportRule,omitempty"`
	ReportPath    string `json:"reportPath,omitempty"`
	AssignSeq     int64  `json:"assignSeq,omitempty"`
	ReportTag     string `json:"reportTag,omitempty"`
	ReportSummary string `json:"reportSummary,omitempty"`
	AgentName     string `json:"agentName,omitempty"`
	// Only on the post row of a verification command (toolEvidenceKinds).
	Evidence *toolEvidence `json:"evidence,omitempty"`
}

type toolLedgerPending struct {
	Start     int64  `json:"start"` // Unix nanoseconds
	Session   string `json:"session"`
	ToolUseID string `json:"toolUseId"`
	Tool      string `json:"tool"`
	Digest    string `json:"digest"`
	Commit    string `json:"commit,omitempty"` // a verification command's HEAD at its start
}

// toolLedgerRoot is the directory holding one ledger directory per agent.
func toolLedgerRoot() string {
	if path := homeSetting("TAILTERM_TOOL_LEDGER_DIR"); path != "" {
		return path
	}
	home, err := homeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "tailterm", "tool-ledger")
}

// toolLedgerAgentOK reports whether an agent id is safe as a directory name.
func toolLedgerAgentOK(agent string) bool {
	if agent == "" || len(agent) > toolLedgerFieldBytes {
		return false
	}
	for i := 0; i < len(agent); i++ {
		c := agent[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// toolLedgerHook runs for all three tool events. The work, stdin read
// included, happens in a goroutine the handler waits on for at most the
// deadline. When the handler returns, cmdHook returns nil and the process
// exits 0, which ends a goroutine still blocked on stdin or the disk.
//
// The identity is the environment's agent, task and run. With no agent in
// the environment it is the owner helper registered from this runtime
// session, resolved from the host helper files inside the same goroutine
// and deadline; without exactly one such helper stdin and the disk are left
// alone.
func toolLedgerHook(e env, _ []string) {
	root := toolLedgerRoot()
	if root == "" || e.agent != "" && (e.task == "" || e.hub == "" || !toolLedgerAgentOK(e.agent)) {
		return
	}
	l := toolLedger{
		started: time.Now(), deadline: toolLedgerDeadline,
		maxInput: toolLedgerMaxInput, maxBytes: toolLedgerMaxBytes, maxPending: toolLedgerMaxPending,
		pendingAge: toolLedgerPendingAge, lockWait: toolLedgerLockWait, now: toolLedgerNow, write: toolLedgerWrite, remove: toolLedgerRemove,
		token: e.token, redactBudget: toolRedactScanBudget, redactMax: toolRedactMaxBytes, stdout: os.Stdout,
		deny: toolDenyCheck, denyMax: toolDenyMaxCommand, scope: toolScopeCheck,
		gate: toolGateOn, name: e.agentName, hub: os.Getenv("TAILTERM_HUB"),
		evidenceBudget: toolEvidenceOutputBudget,
	}
	in := os.Stdin
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }()
		if e.agent == "" {
			helper, ok := offlineOwnerHelper()
			// A partial environment identity must agree with the file.
			if !ok || e.task != "" && e.task != helper.Task || e.runID != "" && e.runID != helper.Run {
				return
			}
			e.task, e.agent, e.runID = helper.Task, helper.Agent, helper.Run
			l.helper, l.name = true, helper.Name
			if time.Since(l.started) >= l.deadline {
				return // the handler has already returned; leave stdin alone
			}
		}
		l.dir, l.task, l.agent, l.run = filepath.Join(root, e.agent), toolLedgerCut(e.task), e.agent, toolLedgerCut(e.runID)
		l.record(in)
	}()
	timer := time.NewTimer(l.deadline)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func (l toolLedger) record(in io.Reader) {
	data, err := io.ReadAll(io.LimitReader(in, l.maxInput+1))
	if time.Since(l.started) >= l.deadline {
		return // the handler has already returned; write nothing late
	}
	var row toolLedgerRow
	oversize := int64(len(data)) > l.maxInput
	var p toolHookInput
	if err != nil || oversize || json.Unmarshal(data, &p) != nil {
		row.Outcome, row.Oversize = "unreadable", oversize
		l.append(row)
		return
	}
	row.Session, row.Tool, row.ToolUseID = toolLedgerCut(p.Session), toolLedgerCut(p.Tool), toolLedgerCut(p.ToolUseID)
	row.ArgsDigest = toolArgsDigest(p.Input)
	// The cwd stays empty when it is absent or not a string. It is read,
	// never stored.
	var cwd string
	_ = json.Unmarshal(p.Cwd, &cwd)
	verify := l.evidenceInput(p.Tool, p.Input)
	switch p.Event {
	case "PreToolUse":
		commit := ""
		if verify.kinds != nil {
			commit = toolEvidenceCommit(cwd)
		}
		l.pre(row, p.Session, commit)
		// After the pending entry is written, so the check cannot lose the pairing.
		if l.deny != nil {
			l.deny(l, row, p.Tool, p.Input)
		}
		if l.scope != nil {
			l.scope(l, row, p.Tool, p.Input, cwd)
		}
		return
	case "PostToolUse":
		row.Outcome = "ok"
		// Off, the default, leaves tool_response undecoded and stdout empty.
		if l.redactMode = toolRedactMode(); l.redactMode != toolRedactOff {
			l.redact(&row, data)
		}
	case "PostToolUseFailure":
		row.Outcome = "error"
		if p.IsInterrupt {
			row.Outcome = "interrupted"
		}
	default:
		row.Session, row.Tool, row.ToolUseID, row.ArgsDigest = "", "", "", ""
		row.Outcome = "unreadable"
		l.append(row)
		return
	}
	entry, claimed := l.claim(p.Session, row)
	switch {
	case p.DurationMS != nil:
		ms := int64(*p.DurationMS)
		row.DurationMs, row.DurationSource = &ms, "claude"
	case claimed && entry.Start > 0:
		ms := (l.now().UnixNano() - entry.Start) / int64(time.Millisecond)
		if ms < 0 {
			ms = 0
		}
		row.DurationMs, row.DurationSource = &ms, "measured"
	}
	if verify.kinds != nil {
		row.Evidence = l.evidence(verify, p, data, entry.Commit, cwd)
	}
	l.append(row)
}

// toolLedgerCut bounds a stored name or id without splitting a character.
func toolLedgerCut(s string) string { return toolLedgerCutTo(s, toolLedgerFieldBytes) }

func toolLedgerCutTo(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// toolArgsDigest is the SHA-256 of tool_input decoded and encoded again by
// encoding/json, which sorts object keys, so key order does not change it.
func toolArgsDigest(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return ""
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(canonical))
}

func (l toolLedger) pendingDir() string { return filepath.Join(l.dir, "pending") }

func toolPendingIDName(session, id string) string {
	sum := sha256.Sum256([]byte(session + "\x00" + id))
	return fmt.Sprintf("i-%x.json", sum[:16])
}

func toolPendingQueuePrefix(session string) string {
	sum := sha256.Sum256([]byte(session))
	return fmt.Sprintf("q-%x-", sum[:8])
}

// pre evicts what the bound requires, then records the call's start. While
// the rows' writes fail nothing is removed, so once a batch of entries beyond
// the bound has built up it records no start; its post still writes the row.
//
// commit is a verification command's HEAD. An entry too large with it is
// written without it, so the start is still recorded and the post reads the
// commit itself.
func (l toolLedger) pre(row toolLedgerRow, session, commit string) {
	if err := os.MkdirAll(l.pendingDir(), 0700); err != nil {
		return
	}
	if l.evict() >= l.maxPending+toolLedgerEvictBatch {
		return
	}
	start := l.now()
	entry := toolLedgerPending{Start: start.UnixNano(), Session: row.Session, ToolUseID: row.ToolUseID, Tool: row.Tool, Digest: row.ArgsDigest, Commit: commit}
	data, err := json.Marshal(entry)
	if err == nil && len(data) > toolLedgerPendingBytes && commit != "" {
		entry.Commit = ""
		data, err = json.Marshal(entry)
	}
	if err != nil || len(data) > toolLedgerPendingBytes {
		return // the post still writes the row from its own input
	}
	if row.ToolUseID != "" {
		l.createPending(toolPendingIDName(session, row.ToolUseID), data, start)
		return
	}
	// Without an id each call gets its own file; names sort by start time.
	for nanos, tries := start.UnixNano(), 0; tries < 8; nanos, tries = nanos+1, tries+1 {
		name := fmt.Sprintf("%s%020d-%d.json", toolPendingQueuePrefix(session), nanos, os.Getpid())
		if l.createPending(name, data, start) {
			return
		}
	}
}

// createPending reports whether it created the file. Its modification time
// is the call's start, which is what eviction orders by.
func (l toolLedger) createPending(name string, data []byte, start time.Time) bool {
	path := filepath.Join(l.pendingDir(), name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return !errors.Is(err, os.ErrExist) // any other failure will not improve on a retry
	}
	_, err = f.Write(data)
	_ = f.Close()
	if err != nil {
		_ = os.Remove(path)
		return true
	}
	_ = os.Chtimes(path, start, start)
	return true
}

// toolPendingFile also matches an eviction claim, "e-" before the name.
func toolPendingFile(name string) bool {
	return (strings.HasPrefix(name, "i-") || strings.HasPrefix(name, "q-") || strings.HasPrefix(name, "e-")) && strings.HasSuffix(name, ".json")
}

// evict writes an unknown row for each pending entry older than the age
// bound, for the oldest ones beyond the count bound less one, so the pre
// that follows stays inside the bound, and for each claim older than the
// grace whatever the bounds. At most a batch per call. It returns the number
// of pending files it leaves.
//
// Each entry is claimed by a rename first, then the rows are appended in
// one write, then the claims are removed, so a pre cut off at any point
// loses no entry. A claim left before the write is evicted again by a later
// pre; one left after it gives a second unknown row for the same call.
func (l toolLedger) evict() int {
	entries, err := os.ReadDir(l.pendingDir())
	if err != nil {
		return 0
	}
	type pendingFile struct {
		name string
		mod  time.Time
	}
	var files []pendingFile
	for _, entry := range entries {
		if !toolPendingFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, pendingFile{entry.Name(), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.Before(files[j].mod)
		}
		return files[i].name < files[j].name
	})
	now := l.now()
	over := len(files) - (l.maxPending - 1)
	var rows []toolLedgerRow
	var claims []string
	for i, file := range files {
		if len(rows) >= toolLedgerEvictBatch {
			break
		}
		// claimPending leaves a claim inside the grace alone.
		if i >= over && now.Sub(file.mod) <= l.pendingAge && !strings.HasPrefix(file.name, "e-") {
			continue
		}
		claim, ok := l.claimPending(file.name, now)
		if !ok {
			continue
		}
		entry, _ := l.readPending(claim) // an unreadable entry is still owned
		rows = append(rows, toolLedgerRow{Session: entry.Session, Tool: entry.Tool, ToolUseID: entry.ToolUseID, ArgsDigest: entry.Digest, Outcome: "unknown"})
		claims = append(claims, claim)
	}
	if !l.append(rows...) {
		return len(files) // the claims stay for a later pre
	}
	for _, claim := range claims {
		_ = l.remove(filepath.Join(l.pendingDir(), claim))
	}
	return len(files) - len(claims)
}

// claimPending renames a pending entry to an eviction claim that names the
// claim time, this process and the entry. Only the process whose rename
// succeeds owns the entry, as a remove does for take. A claim younger than
// the grace may belong to a hook still writing its row, so it is left alone.
func (l toolLedger) claimPending(name string, now time.Time) (string, bool) {
	entryName := name
	if strings.HasPrefix(name, "e-") {
		parts := strings.SplitN(name, "-", 4) // e, claim time, pid, entry name
		if len(parts) == 4 {
			if nanos, err := strconv.ParseInt(parts[1], 10, 64); err == nil && now.Sub(time.Unix(0, nanos)) < toolLedgerClaimGrace {
				return "", false
			}
			entryName = parts[3]
		}
	}
	claim := fmt.Sprintf("e-%020d-%d-%s", now.UnixNano(), os.Getpid(), entryName)
	if os.Rename(filepath.Join(l.pendingDir(), name), filepath.Join(l.pendingDir(), claim)) != nil {
		return "", false
	}
	return claim, true
}

// take reads a pending entry and removes it. Only the process whose remove
// succeeds owns the entry; an unreadable entry is still removed and owned.
func (l toolLedger) take(name string) (toolLedgerPending, bool) {
	entry, _ := l.readPending(name)
	if os.Remove(filepath.Join(l.pendingDir(), name)) != nil {
		return toolLedgerPending{}, false
	}
	return entry, true
}

func (l toolLedger) readPending(name string) (toolLedgerPending, bool) {
	var entry toolLedgerPending
	f, err := os.Open(filepath.Join(l.pendingDir(), name))
	if err != nil {
		return entry, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, toolLedgerPendingBytes+1))
	if err != nil || len(data) > toolLedgerPendingBytes || json.Unmarshal(data, &entry) != nil {
		return toolLedgerPending{}, false
	}
	return entry, true
}

// claim finds the pending entry of a finished call: by tool-use id when the
// input has one, otherwise the session's oldest entry for the same tool and
// arguments.
func (l toolLedger) claim(session string, row toolLedgerRow) (toolLedgerPending, bool) {
	if row.ToolUseID != "" {
		return l.take(toolPendingIDName(session, row.ToolUseID))
	}
	entries, err := os.ReadDir(l.pendingDir()) // sorted by name, so oldest first
	if err != nil {
		return toolLedgerPending{}, false
	}
	prefix := toolPendingQueuePrefix(session)
	for _, file := range entries {
		name := file.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		entry, ok := l.readPending(name)
		if !ok || entry.Tool != row.Tool || entry.Digest != row.ArgsDigest {
			continue
		}
		if entry, ok := l.take(name); ok {
			return entry, true
		}
	}
	return toolLedgerPending{}, false
}

// append writes rows to ledger.jsonl in one write, under the ledger lock
// when it can be had within the lock wait. Rotation happens only under the
// lock, after the size is read again, so two processes cannot both rotate
// and lose the backup. Without the lock the rows are still appended. It
// reports whether the rows were written.
func (l toolLedger) append(rows ...toolLedgerRow) bool {
	if len(rows) == 0 {
		return true
	}
	stamp := l.now().UTC().Format(time.RFC3339Nano)
	var buf bytes.Buffer
	for _, row := range rows {
		row.V, row.Time, row.Task, row.Agent, row.Run = 1, stamp, l.task, l.agent, l.run
		line, err := json.Marshal(row)
		if err != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(l.dir, 0700); err != nil {
		return false
	}
	path := filepath.Join(l.dir, "ledger.jsonl")
	if unlock := l.lock(); unlock != nil {
		defer unlock()
		if info, err := os.Stat(path); err == nil && info.Size() >= l.maxBytes {
			_ = os.Rename(path, path+".1")
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return false
	}
	err = l.write(f, buf.Bytes())
	_ = f.Close()
	return err == nil
}

// lock returns the unlock function, or nil when the lock was not obtained.
// The lock ends with the process, so a killed hook leaves no stale lock.
func (l toolLedger) lock() func() {
	f, err := os.OpenFile(filepath.Join(l.dir, "ledger.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil
	}
	for waited := time.Duration(0); ; waited += toolLedgerLockRetry {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
		}
		if (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN)) || waited >= l.lockWait {
			_ = f.Close()
			return nil
		}
		time.Sleep(toolLedgerLockRetry)
	}
}

// Verification evidence (feature wi_95d33b4e3248bac5): the command text, exit
// code, output digest and commit of a recognised verification command, on
// the call's own post row. Local evidence only.

const (
	toolEvidenceCommandBytes = 16 << 10 // longest command whose text is stored
	toolEvidenceFileBytes    = 4 << 10  // largest .git, HEAD, ref or commondir file read
	toolEvidencePackedBytes  = 8 << 20  // largest packed-refs file read
	toolEvidenceRefDepth     = 5        // symbolic refs followed from HEAD
	toolEvidenceWrappers     = 2        // host-lock exec wrappers looked through
)

// From the handler's start; after it the output is not decoded. A variable so
// tests can change it; a hook call copies it before its goroutine starts.
var toolEvidenceOutputBudget = 120 * time.Millisecond

type toolEvidence struct {
	Kinds           []string `json:"kinds"`
	Command         string   `json:"command,omitempty"`
	CommandRedacted int      `json:"commandRedacted,omitempty"`
	CommandSkip     string   `json:"commandSkip,omitempty"` // oversize | timeout, only when no text
	ExitCode        *int     `json:"exitCode,omitempty"`
	ExitSource      string   `json:"exitSource,omitempty"` // success | error-text
	OutputDigest    string   `json:"outputDigest,omitempty"`
	OutputSource    string   `json:"outputSource,omitempty"` // stdout+stderr | error
	OutputSkip      string   `json:"outputSkip,omitempty"`   // background | unreadable | timeout, only when no digest
	Commit          string   `json:"commit,omitempty"`
	CommitSource    string   `json:"commitSource,omitempty"` // pre | post
}

// toolEvidenceCall is what a Bash call's input says: the verification kinds
// its command runs, none for every other call.
type toolEvidenceCall struct {
	kinds      []string
	command    string
	background bool
}

func (l toolLedger) evidenceInput(tool string, input json.RawMessage) toolEvidenceCall {
	// As wouldDeny: a larger input cannot hold a command within the cap.
	if tool != "Bash" || len(input) > 6*l.denyMax+4096 {
		return toolEvidenceCall{}
	}
	var in struct {
		Command    string          `json:"command"`
		Background json.RawMessage `json:"run_in_background"`
	}
	if json.Unmarshal(input, &in) != nil {
		return toolEvidenceCall{}
	}
	kinds := toolEvidenceKinds(in.Command, l.denyMax)
	if kinds == nil {
		return toolEvidenceCall{}
	}
	return toolEvidenceCall{kinds, in.Command, string(bytes.TrimSpace(in.Background)) == "true"}
}

// toolEvidenceKinds lists the verification kinds a command runs, each once
// in first-seen order, or nil. It reads the simple commands the would-deny
// parser finds, substitutions included, so quoted text and arguments are
// never commands. Text given to a nested bash -c, sh -c or eval is not
// followed. A command over max bytes, or one the parser cannot follow, has
// none.
func toolEvidenceKinds(command string, max int) []string {
	if command == "" || len(command) > max {
		return nil
	}
	p := toolDenyParser{s: command}
	p.parse(0, 0, 0)
	if p.failed {
		return nil
	}
	var kinds []string
	for _, words := range p.cmds {
		kind := toolEvidenceKind(words, 0)
		if kind != "" && !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// toolEvidenceKind classifies one simple command after its assignments and
// wrapper commands are dropped.
func toolEvidenceKind(words []string, wrapped int) string {
	words = toolDenyCommandWords(words)
	if len(words) == 0 {
		return ""
	}
	name, args := toolDenyBase(words[0]), words[1:]
	switch name {
	case "go":
		if len(args) > 1 && args[0] == "-C" {
			args = args[2:]
		} else if len(args) > 0 && strings.HasPrefix(args[0], "-C=") {
			args = args[1:]
		}
		if len(args) > 0 && args[0] == "test" {
			return "go-test"
		}
		if len(args) > 0 && args[0] == "vet" {
			return "go-vet"
		}
	case "npm", "pnpm", "yarn", "bun":
		args = toolEvidenceNpmOptions(args)
		if len(args) > 0 && (args[0] == "test" || args[0] == "t") {
			return "npm-test"
		}
		if len(args) > 0 && (args[0] == "run" || args[0] == "run-script") {
			args = toolEvidenceNpmOptions(args[1:])
			if len(args) > 0 && (args[0] == "test" || strings.HasPrefix(args[0], "test:")) {
				return "npm-test"
			}
		}
	case "node":
		return toolEvidenceNode(args, wrapped)
	case "verify-matrix.mjs":
		return "verify-matrix"
	}
	return ""
}

// toolEvidenceNpmOptions drops leading package manager options, with the
// value of those that take one, as the would-deny check reads them.
func toolEvidenceNpmOptions(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		args = args[1:]
		if _, _, attached := strings.Cut(a, "="); !attached && toolDenyNpmValued[a] && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}

// toolEvidenceNode reads node's options up to the script. --test among them
// is node-test; a script named verify-matrix.mjs is verify-matrix; and the
// host-lock wrapper's exec runs the command after its first "--", which is
// classified the same way.
func toolEvidenceNode(args []string, wrapped int) string {
	test := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			switch a {
			case "--check", "-c", "-e", "--eval", "-p", "--print":
				return "" // a syntax check or an inline script
			case "--test":
				test = true
			}
			if toolDenyNodeValued[a] {
				i++
			}
			continue
		}
		if test {
			return "node-test"
		}
		switch toolDenyBase(a) {
		case "verify-matrix.mjs":
			return "verify-matrix"
		case "verify-matrix-host-lock.mjs":
			if i+1 >= len(args) || args[i+1] != "exec" || wrapped >= toolEvidenceWrappers {
				return ""
			}
			for j := i + 2; j < len(args); j++ {
				if args[j] == "--" {
					return toolEvidenceKind(args[j+1:], wrapped+1)
				}
			}
		}
		return ""
	}
	if test {
		return "node-test" // node --test with no file finds its own
	}
	return ""
}

// evidence builds the field for a verification command's post row. commit
// is the one its pre recorded, if any.
func (l toolLedger) evidence(call toolEvidenceCall, p toolHookInput, data []byte, commit, cwd string) *toolEvidence {
	ev := &toolEvidence{Kinds: call.kinds}
	if len(call.command) > toolEvidenceCommandBytes {
		ev.CommandSkip = "oversize"
	} else if text, n, ok := toolEvidenceRedact(call.command, toolRedactExact(l.token), l.started.Add(l.redactBudget)); ok {
		ev.Command, ev.CommandRedacted = text, n
	} else {
		ev.CommandSkip = "timeout"
	}
	switch {
	case call.background:
		ev.OutputSkip = "background" // what returned is the start, not the command's end
	case time.Since(l.started) >= l.evidenceBudget:
		ev.OutputSkip = "timeout"
		if p.Event == "PostToolUse" {
			ev.ExitCode, ev.ExitSource = new(int), "success"
		}
	default:
		ev.output(p, data)
	}
	if commit != "" {
		ev.Commit, ev.CommitSource = commit, "pre"
	} else if commit = toolEvidenceCommit(cwd); commit != "" {
		ev.Commit, ev.CommitSource = commit, "post"
	}
	return ev
}

// output sets the exit code and the output digest from the post's payload.
// Claude Code gives no numeric exit code: a call that ends with status 0 is
// a PostToolUse, any other a PostToolUseFailure whose error text names it.
func (ev *toolEvidence) output(p toolHookInput, data []byte) {
	var out struct {
		Response json.RawMessage `json:"tool_response"`
		Error    json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &out) != nil {
		ev.OutputSkip = "unreadable"
		return
	}
	if p.Event == "PostToolUse" {
		ev.ExitCode, ev.ExitSource = new(int), "success"
		var resp struct {
			Stdout, Stderr json.RawMessage
		}
		var stdout, stderr string
		if json.Unmarshal(out.Response, &resp) != nil || json.Unmarshal(resp.Stdout, &stdout) != nil ||
			len(resp.Stderr) > 0 && string(resp.Stderr) != "null" && json.Unmarshal(resp.Stderr, &stderr) != nil {
			ev.OutputSkip = "unreadable"
			return
		}
		sum := sha256.New()
		_, _ = io.WriteString(sum, stdout)
		_, _ = sum.Write([]byte{0})
		_, _ = io.WriteString(sum, stderr)
		ev.OutputDigest, ev.OutputSource = fmt.Sprintf("%x", sum.Sum(nil)), "stdout+stderr"
		return
	}
	var msg string
	if json.Unmarshal(out.Error, &msg) != nil {
		ev.OutputSkip = "unreadable"
		return
	}
	ev.OutputDigest, ev.OutputSource = fmt.Sprintf("%x", sha256.Sum256([]byte(msg))), "error"
	if code, ok := toolEvidenceExitCode(msg); ok && !p.IsInterrupt {
		ev.ExitCode, ev.ExitSource = &code, "error-text"
	}
}

// toolEvidenceExitCode reads "Exit code N" or "Error: Exit code N" at the
// start of a failure's text, N from 0 to 255 and followed by the end of the
// text or a space. Any other text has no exit code.
func toolEvidenceExitCode(text string) (int, bool) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(text, "Error: "), "Exit code ")
	if !ok {
		return 0, false
	}
	n := 0
	for n < len(rest) && n < 4 && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 || n > 3 || n < len(rest) && strings.IndexByte(" \t\r\n", rest[n]) < 0 {
		return 0, false
	}
	code, err := strconv.Atoi(rest[:n])
	if err != nil || code > 255 {
		return 0, false
	}
	return code, true
}

// toolEvidenceRedact replaces every span the secret scan finds, groups A, B
// and C alike, with [redacted] and counts them. A scan that did not finish
// gives no text.
func toolEvidenceRedact(text string, exact []string, deadline time.Time) (string, int, bool) {
	rewrite, counted, ok := redactScan(text, exact, deadline)
	if !ok {
		return "", 0, false
	}
	spans := redactMerge(append(rewrite, counted...))
	var out strings.Builder
	at := 0
	for _, span := range spans {
		out.WriteString(text[at:span.start])
		out.WriteString("[redacted]")
		at = span.end
	}
	out.WriteString(text[at:])
	return out.String(), len(spans), true
}

// toolEvidenceCommit is the commit HEAD names in the repository holding cwd,
// read from files alone, as git rev-parse HEAD finds it: the .git directory
// or gitdir file, HEAD, the loose ref in the worktree's git directory then
// the common one, then packed-refs. Only a full lower-case hex id is
// returned; anything else, an unborn branch included, gives "".
func toolEvidenceCommit(cwd string) string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	root := toolScopeRoot(filepath.Clean(cwd))
	if root == "" {
		return ""
	}
	gitdir := filepath.Join(root, ".git")
	info, err := os.Stat(gitdir)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		line, ok := toolEvidenceLine(gitdir)
		target, found := strings.CutPrefix(line, "gitdir: ")
		if !ok || !found || target == "" {
			return ""
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(root, target)
		}
		gitdir = filepath.Clean(target)
	}
	common := gitdir
	if line, ok := toolEvidenceLine(filepath.Join(gitdir, "commondir")); ok && line != "" {
		if !filepath.IsAbs(line) {
			line = filepath.Join(gitdir, line)
		}
		common = filepath.Clean(line)
	}
	head, ok := toolEvidenceLine(filepath.Join(gitdir, "HEAD"))
	for depth := 0; ok && depth < toolEvidenceRefDepth; depth++ {
		ref, symbolic := strings.CutPrefix(head, "ref: ")
		if !symbolic {
			break
		}
		head, ok = toolEvidenceRef(gitdir, common, strings.TrimSpace(ref))
	}
	if !ok || !toolEvidenceHash(head) {
		return ""
	}
	return head
}

// toolEvidenceLine is a small file's first line without surrounding space.
func toolEvidenceLine(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, toolEvidenceFileBytes+1))
	if err != nil || len(data) > toolEvidenceFileBytes {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line), true
}

// toolEvidenceRef reads a ref's value: loose in the worktree's git directory,
// then in the common one, then from packed-refs.
func toolEvidenceRef(gitdir, common, ref string) (string, bool) {
	if !strings.HasPrefix(ref, "refs/") || path.Clean(ref) != ref || strings.ContainsAny(ref, "\x00\\") || strings.Contains(ref, "..") {
		return "", false
	}
	for _, dir := range []string{gitdir, common} {
		if line, ok := toolEvidenceLine(filepath.Join(dir, filepath.FromSlash(ref))); ok {
			return line, true
		}
	}
	f, err := os.Open(filepath.Join(common, "packed-refs"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, toolEvidencePackedBytes))
	sc.Buffer(nil, toolEvidenceFileBytes)
	for sc.Scan() {
		line := sc.Text()
		if hash, name, ok := strings.Cut(line, " "); ok && name == ref && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "^") {
			return hash, true
		}
	}
	return "", false
}

// toolEvidenceHash reports whether s is a full SHA-1 or SHA-256 object id.
func toolEvidenceHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
