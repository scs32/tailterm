package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
)

const (
	toolLedgerEvictBatch   = 32  // unknown rows one pre may write
	toolLedgerPendingBytes = 512 // largest pending file
	toolLedgerFieldBytes   = 128 // largest stored name or id
	toolLedgerLockRetry    = 2 * time.Millisecond
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
	// Secret redaction (tool_redact.go). The token is compared against tool
	// output in memory and never written.
	token        string
	redactMode   string
	redactBudget time.Duration
	redactMax    int
	stdout       io.Writer
}

// toolHookInput holds only the fields the ledger uses. tool_response, error,
// cwd and transcript_path are skipped by the decoder and never kept. Secret
// redaction decodes tool_response itself, and only when it is switched on.
type toolHookInput struct {
	Event       string          `json:"hook_event_name"`
	Session     string          `json:"session_id"`
	Tool        string          `json:"tool_name"`
	ToolUseID   string          `json:"tool_use_id"`
	Input       json.RawMessage `json:"tool_input"`
	DurationMS  *float64        `json:"duration_ms"`
	IsInterrupt bool            `json:"is_interrupt"`
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
}

type toolLedgerPending struct {
	Start     int64  `json:"start"` // Unix nanoseconds
	Session   string `json:"session"`
	ToolUseID string `json:"toolUseId"`
	Tool      string `json:"tool"`
	Digest    string `json:"digest"`
}

// toolLedgerRoot is the directory holding one ledger directory per agent.
func toolLedgerRoot() string {
	if path := os.Getenv("TAILTERM_TOOL_LEDGER_DIR"); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
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
		pendingAge: toolLedgerPendingAge, lockWait: toolLedgerLockWait, now: toolLedgerNow, write: toolLedgerWrite,
		token: e.token, redactBudget: toolRedactScanBudget, redactMax: toolRedactMaxBytes, stdout: os.Stdout,
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
	switch p.Event {
	case "PreToolUse":
		l.pre(row, p.Session)
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
	l.append(row)
}

// toolLedgerCut bounds a stored name or id without splitting a character.
func toolLedgerCut(s string) string {
	if len(s) <= toolLedgerFieldBytes {
		return s
	}
	s = s[:toolLedgerFieldBytes]
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

// pre evicts what the bound requires, then records the call's start.
func (l toolLedger) pre(row toolLedgerRow, session string) {
	if err := os.MkdirAll(l.pendingDir(), 0700); err != nil {
		return
	}
	l.evict()
	start := l.now()
	data, err := json.Marshal(toolLedgerPending{Start: start.UnixNano(), Session: row.Session, ToolUseID: row.ToolUseID, Tool: row.Tool, Digest: row.ArgsDigest})
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

func toolPendingFile(name string) bool {
	return (strings.HasPrefix(name, "i-") || strings.HasPrefix(name, "q-")) && strings.HasSuffix(name, ".json")
}

// evict writes an unknown row for each pending entry older than the age
// bound, and for the oldest ones beyond the count bound less one, so the
// pre that follows stays inside the bound. At most a batch per call.
func (l toolLedger) evict() {
	entries, err := os.ReadDir(l.pendingDir())
	if err != nil {
		return
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
	for i, file := range files {
		if len(rows) >= toolLedgerEvictBatch || (i >= over && now.Sub(file.mod) <= l.pendingAge) {
			break
		}
		entry, ok := l.take(file.name)
		if !ok {
			continue
		}
		rows = append(rows, toolLedgerRow{Session: entry.Session, Tool: entry.Tool, ToolUseID: entry.ToolUseID, ArgsDigest: entry.Digest, Outcome: "unknown"})
	}
	l.append(rows...)
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
// and lose the backup. Without the lock the rows are still appended.
func (l toolLedger) append(rows ...toolLedgerRow) {
	if len(rows) == 0 {
		return
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
		return
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
		return
	}
	_ = l.write(f, buf.Bytes())
	_ = f.Close()
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
