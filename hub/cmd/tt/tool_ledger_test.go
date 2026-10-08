package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const toolLedgerBound = 200 * time.Millisecond // l1: the whole hook

// toolLedgerSandbox points the ledger at a temp directory and returns a
// synthetic identity. No test here needs a hub. The relay state is a temp
// directory too and the runtime thread variables are empty, so a run inside
// a real helper session never reads that session's helper file.
func toolLedgerSandbox(t *testing.T) (env, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tool-ledger")
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(t.TempDir(), "relay"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	// Secret redaction is off: its setting file is one that does not exist,
	// never this host's relay.json.
	t.Setenv("TAILTERM_REDACT_SETTING_FILE", filepath.Join(t.TempDir(), "relay.json"))
	return env{hub: "http://127.0.0.1:9", task: "tsk_ledger", agent: "agt_ledger", runID: "run_ledger"}, root
}

func setToolLedger[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

type toolHookRun struct {
	out     string // stdout and stderr together
	err     error
	elapsed time.Duration
}

// runToolHook runs cmdHook with stdin, stdout and stderr replaced. A nil
// stdin file means the payload is read from a temp file.
func runToolHook(t *testing.T, e env, name, payload string, stdin *os.File) toolHookRun {
	t.Helper()
	dir := t.TempDir()
	if stdin == nil {
		path := filepath.Join(dir, "stdin")
		if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		stdin = f
	}
	sink, err := os.Create(filepath.Join(dir, "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdin, sink, sink
	start := time.Now()
	runErr := cmdHook(e, []string{name})
	elapsed := time.Since(start)
	os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
	out, err := os.ReadFile(sink.Name())
	if err != nil {
		t.Fatal(err)
	}
	return toolHookRun{string(out), runErr, elapsed}
}

func toolPayload(event string, fields map[string]any) string {
	doc := map[string]any{"hook_event_name": event, "cwd": "/work/dir", "transcript_path": "/transcripts/session.jsonl"}
	for k, v := range fields {
		doc[k] = v
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// toolHook runs one tool hook and requires the silent, successful result
// every invocation must have.
func toolHook(t *testing.T, e env, event string, fields map[string]any) {
	t.Helper()
	if r := runToolHook(t, e, "tool", toolPayload(event, fields), nil); r.err != nil || r.out != "" {
		t.Fatalf("%s hook = %v, output %q; want nil and no output", event, r.err, r.out)
	}
}

func toolLedgerRows(t *testing.T, root, agent string, names ...string) []map[string]any {
	t.Helper()
	if len(names) == 0 {
		names = []string{"ledger.jsonl"}
	}
	rows := []map[string]any{}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(root, agent, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var row map[string]any
			if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
				t.Fatalf("%s line %q: %v", name, sc.Text(), err)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func toolPendingFiles(t *testing.T, root, agent string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, agent, "pending"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return entries
}

func requireEmptyRoot(t *testing.T, root string) {
	t.Helper()
	if entries, err := os.ReadDir(root); !errors.Is(err, os.ErrNotExist) && (err != nil || len(entries) != 0) {
		t.Fatalf("ledger root is not empty: %v, %v", entries, err)
	}
}

// a13: only tool and handoff go through the registry; the four existing
// hooks, codex and unknown names take the code that was there before.
func TestHookRegistryHoldsToolAndHandoff(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	if len(hookHandlers) != 2 || hookHandlers["tool"] == nil || hookHandlers["handoff"] == nil {
		t.Fatalf("registry = %v; want exactly tool and handoff", hookHandlers)
	}
	if r := runToolHook(t, e, "nope", "{}", nil); r.err == nil || r.err.Error() != `unknown hook "nope"` {
		t.Fatalf("unknown hook = %v", r.err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, _ = w.WriteString("unread")
	if run := runToolHook(t, env{}, "tool", "", r); run.err != nil || run.out != "" || run.elapsed > toolLedgerBound {
		t.Fatalf("tool hook with no identity = %+v", run)
	}
	w.Close()
	if left, _ := io.ReadAll(r); string(left) != "unread" {
		t.Fatalf("stdin was read with no identity: %q left", left)
	}
	requireEmptyRoot(t, root)
}

// a1: the hook returns nil, silently, inside the bound whatever stdin, the
// disk or the lock do. A timing miss is retried twice, since a stalled test
// host is not the property; every attempt must still return nil and silent.
//
// The row is a separate expectation (bug wi_4f795d41e0038d04). A hook that
// reaches its own deadline returns on time and drops the row, by design, so
// on a loaded host a slow attempt can be inside the bound and leave nothing.
// That attempt is retried too. An attempt that returned before the deadline
// saw its work finish, and must have left exactly the expected row.
func TestToolLedgerNeverBlocks(t *testing.T) {
	pre := toolPayload("PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}})
	post := toolPayload("PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}})
	type result struct {
		run  toolHookRun
		rows []map[string]any
	}
	cases := []struct {
		name string
		// stalls marks a case that holds the hook until its deadline on
		// every attempt, so its row check is the deadline outcome itself.
		stalls bool
		run    func(t *testing.T, e env, root string) toolHookRun
		check  func(t *testing.T, rows []map[string]any)
	}{
		{"stdin held open", true, func(t *testing.T, e env, root string) toolHookRun {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { w.Close(); r.Close() })
			_, _ = w.WriteString(pre[:len(pre)/2])
			return runToolHook(t, e, "tool", "", r)
		}, func(t *testing.T, rows []map[string]any) {
			if len(rows) != 0 {
				t.Fatalf("rows from an unfinished input: %v", rows)
			}
		}},
		{"write blocks", false, func(t *testing.T, e env, root string) toolHookRun {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			setToolLedger(t, &toolLedgerWrite, func(*os.File, []byte) error {
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
				return errors.New("blocked")
			})
			return runToolHook(t, e, "tool", post, nil)
		}, nil},
		{"write panics", false, func(t *testing.T, e env, root string) toolHookRun {
			setToolLedger(t, &toolLedgerWrite, func(*os.File, []byte) error { panic("disk on fire") })
			return runToolHook(t, e, "tool", post, nil)
		}, nil},
		{"unwritable ledger directory", false, func(t *testing.T, e env, root string) toolHookRun {
			if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			return runToolHook(t, e, "tool", post, nil)
		}, nil},
		{"lock held elsewhere", false, func(t *testing.T, e env, root string) toolHookRun {
			if err := os.MkdirAll(filepath.Join(root, e.agent), 0700); err != nil {
				t.Fatal(err)
			}
			held, err := os.OpenFile(filepath.Join(root, e.agent, "ledger.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { held.Close() })
			if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			return runToolHook(t, e, "tool", post, nil)
		}, func(t *testing.T, rows []map[string]any) {
			if len(rows) != 1 || rows[0]["outcome"] != "ok" {
				t.Fatalf("row written past a held lock = %v", rows)
			}
		}},
		{"empty input", false, func(t *testing.T, e env, root string) toolHookRun {
			return runToolHook(t, e, "tool", "", nil)
		}, func(t *testing.T, rows []map[string]any) {
			if len(rows) != 1 || rows[0]["outcome"] != "unreadable" || rows[0]["oversize"] != nil {
				t.Fatalf("rows = %v", rows)
			}
		}},
		{"malformed input", false, func(t *testing.T, e env, root string) toolHookRun {
			return runToolHook(t, e, "tool", `{"hook_event_name":"PreToolUse","tool_name":`, nil)
		}, func(t *testing.T, rows []map[string]any) {
			if len(rows) != 1 || rows[0]["outcome"] != "unreadable" || rows[0]["tool"] != "" {
				t.Fatalf("rows = %v", rows)
			}
		}},
		{"9 MiB input", false, func(t *testing.T, e env, root string) toolHookRun {
			return runToolHook(t, e, "tool", toolPayload("PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Read", "tool_response": strings.Repeat("r", 9<<20)}), nil)
		}, func(t *testing.T, rows []map[string]any) {
			if len(rows) != 1 || rows[0]["outcome"] != "unreadable" || rows[0]["oversize"] != true || rows[0]["tool"] != "" {
				t.Fatalf("rows = %v", rows)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var last result
			deadline := toolLedgerDeadline
			for attempt := 0; attempt < 3; attempt++ {
				t.Run(strconv.Itoa(attempt), func(t *testing.T) {
					e, root := toolLedgerSandbox(t)
					last.run = c.run(t, e, root)
					last.rows = nil
					if info, err := os.Stat(root); err == nil && info.IsDir() {
						last.rows = toolLedgerRows(t, root, e.agent)
					}
				})
				if last.run.err != nil || last.run.out != "" {
					t.Fatalf("hook = %v, output %q; want nil and no output", last.run.err, last.run.out)
				}
				if last.run.elapsed >= toolLedgerBound {
					continue // a timing miss
				}
				if c.check == nil {
					return
				}
				// Inside the bound. Only a hook that reached its deadline and
				// left nothing has no row to judge; a row it did leave, and
				// every row of a faster attempt, is checked in full.
				if !c.stalls && last.run.elapsed >= deadline && len(last.rows) == 0 {
					continue
				}
				c.check(t, last.rows)
				return
			}
			if last.run.elapsed >= toolLedgerBound {
				t.Fatalf("hook took %v three times; want under %v", last.run.elapsed, toolLedgerBound)
			}
			t.Fatalf("no attempt in three left a row to check; the last reached the hook's %v deadline in %v and left none", deadline, last.run.elapsed)
		})
	}
}

// a2: nothing on stdout or stderr for any event, so nothing Claude Code
// could read as a decision.
func TestToolLedgerPrintsNothing(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	call := map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}}
	for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure", "SomethingNew"} {
		if r := runToolHook(t, e, "tool", toolPayload(event, call), nil); r.err != nil || r.out != "" {
			t.Errorf("%s = %v, output %q", event, r.err, r.out)
		}
	}
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != 3 || rows[2]["outcome"] != "unreadable" {
		t.Fatalf("rows = %v", rows)
	}
}

// a3: without a Tailterm agent identity the hook leaves stdin and the disk alone.
func TestToolLedgerNoopOutsideSession(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	for name, outside := range map[string]env{
		"no agent":     {hub: e.hub, task: e.task},
		"no task":      {hub: e.hub, agent: e.agent},
		"no hub":       {task: e.task, agent: e.agent},
		"unsafe agent": {hub: e.hub, task: e.task, agent: "../agt_ledger"},
	} {
		for _, event := range []string{"PreToolUse", "PostToolUse"} {
			payload := toolPayload(event, map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash"})
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.WriteString(payload)
			w.Close()
			run := runToolHook(t, outside, "tool", "", r)
			left, _ := io.ReadAll(r)
			r.Close()
			if run.err != nil || run.out != "" || string(left) != payload {
				t.Errorf("%s %s = %v, output %q, %d of %d stdin bytes left", name, event, run.err, run.out, len(left), len(payload))
			}
		}
	}
	requireEmptyRoot(t, root)
	if entries, _ := os.ReadDir(filepath.Dir(root)); len(entries) != 0 {
		t.Fatalf("an unsafe agent id wrote beside the ledger root: %v", entries)
	}
}

var toolDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// a5: one row per call with every field.
func TestToolLedgerOneRowPerCall(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	input := map[string]any{"command": "go test ./...", "timeout": 5000}
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": input})
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != 0 {
		t.Fatalf("a pre alone wrote rows: %v", rows)
	}
	toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": input, "tool_response": map[string]any{"stdout": "ok"}, "duration_ms": 1234})
	rows := toolLedgerRows(t, root, e.agent)
	if len(rows) != 1 {
		t.Fatalf("rows after one call = %v", rows)
	}
	digest, _ := rows[0]["argsDigest"].(string)
	want := map[string]any{"v": float64(1), "time": rows[0]["time"], "task": "tsk_ledger", "agent": "agt_ledger", "run": "run_ledger", "session": "s1", "tool": "Bash", "toolUseId": "u1", "argsDigest": digest, "outcome": "ok", "durationMs": float64(1234), "durationSource": "claude"}
	if fmt.Sprint(rows[0]) != fmt.Sprint(want) || !toolDigestRE.MatchString(digest) {
		t.Fatalf("row = %v\nwant  %v", rows[0], want)
	}
	if stamp, err := time.Parse(time.RFC3339Nano, rows[0]["time"].(string)); err != nil || stamp.Location() != time.UTC {
		t.Fatalf("time %v is not UTC RFC 3339: %v", rows[0]["time"], err)
	}
	if left := toolPendingFiles(t, root, e.agent); len(left) != 0 {
		t.Fatalf("pending entries left after the post: %v", left)
	}

	// Two interleaved calls, one failing and one interrupted; no duration_ms,
	// so each duration is measured from its own pre.
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	setToolLedger(t, &toolLedgerNow, func() time.Time { return now })
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u2", "tool_name": "Bash", "tool_input": input})
	now = now.Add(time.Second)
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u3", "tool_name": "Read", "tool_input": map[string]any{"file_path": "/x"}})
	now = now.Add(time.Second)
	toolHook(t, e, "PostToolUseFailure", map[string]any{"session_id": "s1", "tool_use_id": "u2", "tool_name": "Bash", "tool_input": input, "error": "exit 1"})
	now = now.Add(time.Second)
	toolHook(t, e, "PostToolUseFailure", map[string]any{"session_id": "s1", "tool_use_id": "u3", "tool_name": "Read", "tool_input": map[string]any{"file_path": "/x"}, "error": "stopped", "is_interrupt": true})
	// A post with no pre still gives a row, with keys in another order.
	toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u4", "tool_name": "Bash", "tool_input": json.RawMessage(`{"timeout":5000,"command":"go test ./..."}`)})
	rows = toolLedgerRows(t, root, e.agent)
	if len(rows) != 4 {
		t.Fatalf("rows = %v", rows)
	}
	for i, want := range []struct {
		id, tool, outcome string
		ms                any
		source            any
	}{{"u2", "Bash", "error", float64(2000), "measured"}, {"u3", "Read", "interrupted", float64(2000), "measured"}, {"u4", "Bash", "ok", nil, nil}} {
		row := rows[i+1]
		if row["toolUseId"] != want.id || row["tool"] != want.tool || row["outcome"] != want.outcome || row["durationMs"] != want.ms || row["durationSource"] != want.source {
			t.Errorf("row %d = %v; want %+v", i+1, row, want)
		}
	}
	if rows[1]["argsDigest"] != digest || rows[3]["argsDigest"] != digest || rows[2]["argsDigest"] == digest {
		t.Errorf("digests do not follow the arguments: %v %v %v, first %v", rows[1]["argsDigest"], rows[2]["argsDigest"], rows[3]["argsDigest"], digest)
	}
	if rows[1]["time"] != "2026-10-06T12:00:02Z" {
		t.Errorf("row time = %v", rows[1]["time"])
	}
	if left := toolPendingFiles(t, root, e.agent); len(left) != 0 {
		t.Fatalf("pending entries left: %v", left)
	}
}

// a5: calls between the old 1 MiB read and the 8 MiB cap pair into one
// complete row; above the cap the row is unreadable and the pre stays pending.
func TestToolLedgerLargeCall(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second) // pairing is the subject here, not timing
	input := map[string]any{"file_path": "/big", "content": strings.Repeat("c", 2<<20)}
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "big", "tool_name": "Write", "tool_input": input})
	toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "big", "tool_name": "Write", "tool_input": input, "tool_response": strings.Repeat("r", 4<<20)})
	rows := toolLedgerRows(t, root, e.agent)
	if len(rows) != 1 || rows[0]["tool"] != "Write" || rows[0]["toolUseId"] != "big" || rows[0]["outcome"] != "ok" || rows[0]["durationSource"] != "measured" || !toolDigestRE.MatchString(fmt.Sprint(rows[0]["argsDigest"])) {
		t.Fatalf("large call rows = %v", rows)
	}
	if left := toolPendingFiles(t, root, e.agent); len(left) != 0 {
		t.Fatalf("large call left pending entries: %v", left)
	}
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "huge", "tool_name": "Read", "tool_input": map[string]any{"file_path": "/huge"}})
	toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "huge", "tool_name": "Read", "tool_input": map[string]any{"file_path": "/huge"}, "tool_response": strings.Repeat("r", 9<<20)})
	rows = toolLedgerRows(t, root, e.agent)
	if len(rows) != 2 || rows[1]["outcome"] != "unreadable" || rows[1]["oversize"] != true || rows[1]["tool"] != "" || rows[1]["toolUseId"] != "" || rows[1]["argsDigest"] != "" || rows[1]["agent"] != e.agent {
		t.Fatalf("rows after a 9 MiB post = %v", rows)
	}
	if left := toolPendingFiles(t, root, e.agent); len(left) != 1 {
		t.Fatalf("pending after a 9 MiB post = %v; want the pre kept", left)
	}
}

// a5: without a tool-use id, identical overlapping calls are separate
// pending entries claimed in start order, and sessions do not cross.
func TestToolLedgerNoToolUseID(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	setToolLedger(t, &toolLedgerNow, func() time.Time { return now })
	call := func(session string) map[string]any {
		return map[string]any{"session_id": session, "tool_name": "Bash", "tool_input": map[string]any{"command": "sleep 1"}}
	}
	toolHook(t, e, "PreToolUse", call("s1"))
	toolHook(t, e, "PreToolUse", call("s1")) // same instant: still its own file
	now = now.Add(time.Second)
	toolHook(t, e, "PreToolUse", call("s1"))
	if pending := toolPendingFiles(t, root, e.agent); len(pending) != 3 {
		t.Fatalf("pending = %v; want three separate entries", pending)
	}
	now = now.Add(9 * time.Second)
	toolHook(t, e, "PostToolUse", call("s1"))
	toolHook(t, e, "PostToolUse", call("s1"))
	now = now.Add(2 * time.Second)
	toolHook(t, e, "PostToolUse", call("s1"))
	rows := toolLedgerRows(t, root, e.agent)
	if len(rows) != 3 || rows[0]["durationMs"] != float64(10000) || rows[1]["durationMs"] != float64(10000) || rows[2]["durationMs"] != float64(11000) || rows[2]["durationSource"] != "measured" || rows[0]["toolUseId"] != "" {
		t.Fatalf("rows = %v; want starts claimed oldest first", rows)
	}
	if pending := toolPendingFiles(t, root, e.agent); len(pending) != 0 {
		t.Fatalf("pending left = %v", pending)
	}

	toolHook(t, e, "PreToolUse", call("s2"))
	toolHook(t, e, "PostToolUse", call("s1")) // must not take s2's entry
	rows = toolLedgerRows(t, root, e.agent)
	if len(rows) != 4 || rows[3]["session"] != "s1" || rows[3]["durationMs"] != nil {
		t.Fatalf("a post claimed another session's entry: %v", rows[len(rows)-1])
	}
	if pending := toolPendingFiles(t, root, e.agent); len(pending) != 1 {
		t.Fatalf("pending = %v; want the other session's entry kept", pending)
	}
	// A different tool or arguments in the same session does not match either.
	other := call("s2")
	other["tool_input"] = map[string]any{"command": "sleep 2"}
	toolHook(t, e, "PostToolUse", other)
	if pending := toolPendingFiles(t, root, e.agent); len(pending) != 1 {
		t.Fatalf("a post with other arguments claimed the entry: %v", pending)
	}
}

// a5: directories 0700, files 0600.
func TestToolLedgerIsPrivate(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash"})
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_name": "Bash"})
	toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u9", "tool_name": "Bash"})
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0600)
		if d.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %v; want %v", path, info.Mode().Perm(), want)
		}
		seen++
		return nil
	})
	if err != nil || seen != 7 { // root, agent, pending, two entries, ledger, lock
		t.Fatalf("walked %d paths, %v", seen, err)
	}
}

// a6: no raw arguments, responses, errors or paths anywhere under the root.
func TestToolLedgerStoresNoRawContent(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	markers := []string{"MARK-INPUT", "MARK-RESPONSE", "MARK-ERROR", "MARK-CWD", "MARK-TRANSCRIPT"}
	fields := func(id string) map[string]any {
		return map[string]any{"session_id": "s1", "tool_use_id": id, "tool_name": "Bash",
			"tool_input":    map[string]any{"command": "echo MARK-INPUT", "env": map[string]any{"TOKEN": "MARK-INPUT"}},
			"tool_response": map[string]any{"stdout": "MARK-RESPONSE"}, "error": "MARK-ERROR",
			"cwd": "/MARK-CWD", "transcript_path": "/MARK-TRANSCRIPT.jsonl"}
	}
	toolHook(t, e, "PreToolUse", fields("u1"))
	toolHook(t, e, "PostToolUse", fields("u1"))
	toolHook(t, e, "PreToolUse", fields("u2"))
	toolHook(t, e, "PostToolUseFailure", fields("u2"))
	toolHook(t, e, "PreToolUse", fields("u3")) // stays pending
	noID := fields("")
	delete(noID, "tool_use_id")
	toolHook(t, e, "PreToolUse", noID)                                                                        // stays pending, queue form
	if r := runToolHook(t, e, "tool", `{"tool_input":"MARK-INPUT","error":"MARK-ERROR"`, nil); r.err != nil { // malformed
		t.Fatal(r.err)
	}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for _, marker := range markers {
			if bytes.Contains(data, []byte(marker)) || strings.Contains(path, marker) {
				t.Errorf("%s holds %s", path, marker)
			}
		}
		return nil
	})
	if err != nil || files != 4 { // ledger, lock, two pending
		t.Fatalf("read %d files, %v", files, err)
	}
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
}

// a7: the ledger rotates at the bound into exactly one backup.
func TestToolLedgerRotates(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	setToolLedger(t, &toolLedgerMaxBytes, 1024)
	for i := 0; i < 40; i++ {
		toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": fmt.Sprintf("u%d", i), "tool_name": "Bash"})
	}
	entries, err := os.ReadDir(filepath.Join(root, e.agent))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if fmt.Sprint(names) != "[ledger.jsonl ledger.jsonl.1 ledger.lock]" {
		t.Fatalf("ledger directory = %v", names)
	}
	// The size is checked before a row is added, so the file may pass the
	// bound by the one row that crosses it.
	rows := toolLedgerRows(t, root, e.agent)
	line, _ := json.Marshal(rows[0])
	for _, name := range []string{"ledger.jsonl", "ledger.jsonl.1"} {
		if info, _ := os.Stat(filepath.Join(root, e.agent, name)); info.Size() >= 1024+int64(len(line))+1 {
			t.Errorf("%s is %d bytes; want under the bound plus one row", name, info.Size())
		}
	}
	if last := rows[len(rows)-1]["toolUseId"]; last != "u39" {
		t.Fatalf("newest row = %v", last)
	}
	if backup := toolLedgerRows(t, root, e.agent, "ledger.jsonl.1"); len(backup) == 0 || len(backup)+len(rows) > 40 {
		t.Fatalf("backup holds %d rows beside %d", len(backup), len(rows))
	}
}

const (
	toolLedgerChildEnv  = "TT_TEST_TOOL_LEDGER_CHILD"
	toolLedgerChildren  = 8
	toolLedgerChildRows = 200
	toolLedgerRaceBound = 250_000
)

// a7: processes appending and rotating at once lose no row.
func TestToolLedgerRotationAcrossProcesses(t *testing.T) {
	if child := os.Getenv(toolLedgerChildEnv); child != "" {
		// The parent set the ledger root in the child's environment, which
		// TestMain then cleared with every other TAILTERM_ variable.
		t.Setenv("TAILTERM_TOOL_LEDGER_DIR", os.Getenv(toolLedgerChildEnv+"_DIR"))
		e := env{hub: "http://127.0.0.1:9", task: "tsk_ledger", agent: "agt_ledger", runID: "run_ledger"}
		setToolLedger(t, &toolLedgerMaxBytes, toolLedgerRaceBound)
		setToolLedger(t, &toolLedgerDeadline, 30*time.Second)
		for i := 0; i < toolLedgerChildRows; i++ {
			toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": fmt.Sprintf("c%s-%d", child, i), "tool_name": "Bash", "tool_input": map[string]any{"n": i}, "duration_ms": i})
		}
		return
	}
	root := filepath.Join(t.TempDir(), "tool-ledger")
	cmds := make([]*exec.Cmd, toolLedgerChildren)
	outs := make([]bytes.Buffer, toolLedgerChildren)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0], "-test.run=^TestToolLedgerRotationAcrossProcesses$", "-test.count=1")
		cmds[i].Env = append(os.Environ(), toolLedgerChildEnv+"="+strconv.Itoa(i), toolLedgerChildEnv+"_DIR="+root)
		cmds[i].Stdout, cmds[i].Stderr = &outs[i], &outs[i]
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, outs[i].String())
		}
	}
	seen := map[string]int{}
	for _, row := range toolLedgerRows(t, root, "agt_ledger", "ledger.jsonl.1", "ledger.jsonl") { // also fails on a line that does not parse
		seen[fmt.Sprint(row["toolUseId"])]++
	}
	for child := 0; child < toolLedgerChildren; child++ {
		for i := 0; i < toolLedgerChildRows; i++ {
			if id := fmt.Sprintf("c%d-%d", child, i); seen[id] != 1 {
				t.Errorf("row %s appears %d times", id, seen[id])
			}
		}
	}
	if len(seen) != toolLedgerChildren*toolLedgerChildRows {
		t.Errorf("%d distinct rows; want %d", len(seen), toolLedgerChildren*toolLedgerChildRows)
	}
	backup, err := os.Stat(filepath.Join(root, "agt_ledger", "ledger.jsonl.1"))
	if err != nil {
		t.Fatalf("no rotation happened: %v", err)
	}
	current, err := os.Stat(filepath.Join(root, "agt_ledger", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if total := backup.Size() + current.Size(); backup.Size() < toolLedgerRaceBound || total <= toolLedgerRaceBound || total >= 2*toolLedgerRaceBound {
		t.Errorf("backup %d + current %d bytes; want a backup of at least %d and a total between one and two bounds", backup.Size(), current.Size(), toolLedgerRaceBound)
	}
}

// a7: pending files are bounded in number, size and age.
func TestToolLedgerPendingIsBounded(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	setToolLedger(t, &toolLedgerNow, func() time.Time { return now })
	for i := 0; i < 300; i++ {
		now = now.Add(time.Millisecond)
		toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": fmt.Sprintf("u%d", i), "tool_name": "Bash", "tool_input": map[string]any{"n": i}})
	}
	pending := toolPendingFiles(t, root, e.agent)
	if len(pending) != 256 {
		t.Fatalf("%d pending files; want 256", len(pending))
	}
	for _, entry := range pending {
		if info, err := entry.Info(); err != nil || info.Size() > 512 {
			t.Fatalf("pending %s: %v, %v", entry.Name(), info, err)
		}
	}
	rows := toolLedgerRows(t, root, e.agent)
	if len(rows) != 44 {
		t.Fatalf("%d rows; want 44 unknown", len(rows))
	}
	for i, row := range rows { // oldest first, with what the pre recorded
		if row["outcome"] != "unknown" || row["toolUseId"] != fmt.Sprintf("u%d", i) || row["tool"] != "Bash" || row["agent"] != e.agent || row["run"] != e.runID || row["session"] != "s1" || !toolDigestRE.MatchString(fmt.Sprint(row["argsDigest"])) || row["durationMs"] != nil {
			t.Fatalf("row %d = %v", i, row)
		}
	}

	// Past 24 hours every entry is evicted, at most 32 on each pre.
	now = now.Add(25 * time.Hour)
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "late", "tool_name": "Bash"})
	if rows, pending := toolLedgerRows(t, root, e.agent), toolPendingFiles(t, root, e.agent); len(rows) != 44+32 || len(pending) != 256-32+1 || rows[44]["toolUseId"] != "u44" {
		t.Fatalf("after an aged pre: %d rows, %d pending", len(rows), len(pending))
	}

	// Fields that cannot fit the 512 bytes leave no entry; ids are cut.
	_, root2 := toolLedgerSandbox(t)
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": strings.Repeat("\"", 200), "tool_use_id": strings.Repeat("\"", 200), "tool_name": strings.Repeat("\"", 200)})
	toolHook(t, e, "PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": strings.Repeat("i", 4000), "tool_name": strings.Repeat("é", 4000)})
	pending = toolPendingFiles(t, root2, e.agent)
	if len(pending) != 1 {
		t.Fatalf("pending = %v; want only the entry that fits", pending)
	}
	if info, _ := pending[0].Info(); info.Size() > 512 {
		t.Fatalf("pending entry is %d bytes", info.Size())
	}
	toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": strings.Repeat("i", 4000), "tool_name": strings.Repeat("é", 4000)})
	rows = toolLedgerRows(t, root2, e.agent)
	if len(rows) != 1 || len(rows[0]["toolUseId"].(string)) != 128 || rows[0]["tool"] != strings.Repeat("é", 64) || rows[0]["durationSource"] != "measured" {
		t.Fatalf("row with long fields = %v", rows)
	}
}

// a12: the hook makes no hub request, so a closed or silent hub cannot slow it.
func TestToolLedgerIgnoresHub(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var connections atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			defer conn.Close() // accepted and never answered
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	for name, hub := range map[string]string{"closed port": closedURL, "silent listener": "http://" + ln.Addr().String()} {
		t.Run(name, func(t *testing.T) {
			e, root := toolLedgerSandbox(t)
			e.hub = hub
			call := map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "duration_ms": 5}
			slowest := time.Duration(0)
			for _, event := range []string{"PreToolUse", "PostToolUse"} {
				r := runToolHook(t, e, "tool", toolPayload(event, call), nil)
				if r.err != nil || r.out != "" {
					t.Fatalf("%s = %v, output %q", event, r.err, r.out)
				}
				slowest = max(slowest, r.elapsed)
			}
			if slowest >= toolLedgerBound {
				t.Errorf("slowest hook took %v; want under %v", slowest, toolLedgerBound)
			}
			if rows := toolLedgerRows(t, root, e.agent); len(rows) != 1 || rows[0]["outcome"] != "ok" {
				t.Fatalf("rows = %v", rows)
			}
		})
	}
	if n := connections.Load(); n != 0 {
		t.Fatalf("the hook opened %d hub connection(s)", n)
	}
}

// The canonical form does not depend on key order or spacing, and an absent
// tool_input has no digest.
func TestToolArgsDigest(t *testing.T) {
	a := toolArgsDigest(json.RawMessage(`{"b":[1,2.50,{"y":null,"x":"é"}],"a":12345678901234567890}`))
	b := toolArgsDigest(json.RawMessage(` { "a" : 12345678901234567890, "b" : [ 1, 2.50, { "x" : "é", "y" : null } ] } `))
	if a != b || !toolDigestRE.MatchString(a) {
		t.Fatalf("digests differ: %q %q", a, b)
	}
	got := []string{toolArgsDigest(nil), toolArgsDigest(json.RawMessage(" ")), toolArgsDigest(json.RawMessage(`{"a":1}`)), toolArgsDigest(json.RawMessage(`{"a":2}`))}
	sort.Strings(got)
	if got[0] != "" || got[1] != "" || got[2] == got[3] || got[2] == "" {
		t.Fatalf("digests = %q", got)
	}
}

// The owner helper's session (bug wi_b3e8ecd5a2ee01b3): no Tailterm identity
// in the environment, a helper file on the host, and the runtime's own
// thread variable.

const (
	helperLedgerHub    = "http://127.0.0.1:9"
	helperLedgerTask   = "tsk_00000000000000a1"
	helperLedgerAgent  = "agt_00000000000000a1"
	helperLedgerRun    = "run_helper_a1"
	helperLedgerThread = "11111111-1111-4111-8111-111111111111"
)

// helperLedgerSandbox gives a host with no Tailterm identity and no hub
// configuration: an empty home, temp relay state and ledger root, and every
// identity variable empty. It returns the ledger root and the relay directory.
func helperLedgerSandbox(t *testing.T) (root, relay string) {
	t.Helper()
	dir := t.TempDir()
	root, relay = filepath.Join(dir, "tool-ledger"), filepath.Join(dir, "relay")
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
	t.Setenv("TAILTERM_RELAY_STATE", relay)
	for _, name := range []string{"TAILTERM_AGENT", "TAILTERM_TASK", "TAILTERM_RUN", "TAILTERM_HUB", "TAILTERM_TOKEN", "TAILTERM_AGENT_NAME", "TAILTERM_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		t.Setenv(name, "")
	}
	if err := os.MkdirAll(relay, 0700); err != nil {
		t.Fatal(err)
	}
	return root, relay
}

func helperLedgerFile() ownerHelperFile {
	return ownerHelperFile{Hub: helperLedgerHub, Task: helperLedgerTask, Agent: helperLedgerAgent, Name: "owner-helper", Run: helperLedgerRun, Thread: helperLedgerThread, Runtime: "claude"}
}

// writeHelperLedgerFile stores f where tt helper register would.
func writeHelperLedgerFile(t *testing.T, f ownerHelperFile) {
	t.Helper()
	if err := writePrivateJSON(ownerHelperPath(f.Hub, f.Task), f); err != nil {
		t.Fatal(err)
	}
}

// helperLedgerEnv is the environment tt reads in the sandbox. It fails the
// test unless that environment carries no agent, task or run.
func helperLedgerEnv(t *testing.T) env {
	t.Helper()
	e := readEnv()
	if e.agent != "" || e.task != "" || e.runID != "" {
		t.Fatalf("the sandbox environment has an identity: agent %q, task %q, run %q", e.agent, e.task, e.runID)
	}
	return e
}

func rowKeys(row map[string]any) string {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// a1: with no agent, task or run in the environment, the helper's calls are
// written under the helper file's agent, task and run, in the same row
// format as any agent's, and the file's hub is never contacted.
func TestToolLedgerHelperSession(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var connections atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			defer conn.Close() // accepted and never answered
		}
	}()
	for runtime, variable := range map[string]string{"claude": "CLAUDE_CODE_SESSION_ID", "codex": "CODEX_THREAD_ID"} {
		t.Run(runtime, func(t *testing.T) {
			root, _ := helperLedgerSandbox(t)
			file := helperLedgerFile()
			file.Hub, file.Runtime = "http://"+ln.Addr().String(), runtime
			// Private registration fields the ledger has no use for.
			file.Registration, file.RequestID, file.RequestHash, file.Session, file.SessionID = "ohr_private", "request-private", "hash-private", "Codex_Orchestrator", "$7"
			writeHelperLedgerFile(t, file)
			t.Setenv(variable, file.Thread)
			e := helperLedgerEnv(t)
			call := map[string]any{"session_id": file.Thread, "tool_use_id": "u1", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}, "duration_ms": 7}
			toolHook(t, e, "PreToolUse", call)
			toolHook(t, e, "PostToolUse", call)
			rows := toolLedgerRows(t, root, file.Agent)
			if len(rows) != 1 {
				t.Fatalf("helper rows = %v", rows)
			}
			digest, _ := rows[0]["argsDigest"].(string)
			want := map[string]any{"v": float64(1), "time": rows[0]["time"], "task": file.Task, "agent": file.Agent, "run": file.Run, "session": file.Thread, "tool": "Bash", "toolUseId": "u1", "argsDigest": digest, "outcome": "ok", "durationMs": float64(7), "durationSource": "claude"}
			if fmt.Sprint(rows[0]) != fmt.Sprint(want) || !toolDigestRE.MatchString(digest) {
				t.Fatalf("row = %v\nwant  %v", rows[0], want)
			}
			if left := toolPendingFiles(t, root, file.Agent); len(left) != 0 {
				t.Fatalf("pending entries left after the post: %v", left)
			}
			// The same call under an environment identity has the same keys.
			agent := env{hub: file.Hub, task: "tsk_ledger", agent: "agt_ledger", runID: "run_ledger"}
			toolHook(t, agent, "PreToolUse", call)
			toolHook(t, agent, "PostToolUse", call)
			if other := toolLedgerRows(t, root, agent.agent); len(other) != 1 || rowKeys(other[0]) != rowKeys(rows[0]) {
				t.Fatalf("agent rows = %v; want one row with the helper row's keys %s", other, rowKeys(rows[0]))
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 2 {
				t.Fatalf("ledger root = %v, %v; want the helper and the agent", entries, err)
			}
		})
	}
	if n := connections.Load(); n != 0 {
		t.Fatalf("the hook opened %d hub connection(s)", n)
	}
}

// a2: without exactly one helper file for this runtime session the hook
// returns nil, prints nothing, leaves stdin unread and writes nothing.
func TestToolLedgerHelperNoMatch(t *testing.T) {
	const otherThread = "22222222-2222-4222-8222-222222222222"
	claude := func(t *testing.T) { t.Setenv("CLAUDE_CODE_SESSION_ID", helperLedgerThread) }
	missing := func(change func(*ownerHelperFile)) func(*testing.T, string) {
		return func(t *testing.T, _ string) {
			file := helperLedgerFile()
			change(&file)
			writeHelperLedgerFile(t, file)
			claude(t)
		}
	}
	// misnamed stores a complete, matching helper file under a name its own
	// hub and task do not give it.
	misnamed := func(t *testing.T, relay, name string) {
		data, err := json.Marshal(helperLedgerFile())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(relay, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		claude(t)
	}
	cases := map[string]func(t *testing.T, relay string){
		"no helper file": func(t *testing.T, _ string) { claude(t) },
		"no thread variable": func(t *testing.T, _ string) {
			writeHelperLedgerFile(t, helperLedgerFile())
		},
		"thread differs": func(t *testing.T, _ string) {
			writeHelperLedgerFile(t, helperLedgerFile())
			t.Setenv("CLAUDE_CODE_SESSION_ID", otherThread)
		},
		"runtime differs":       missing(func(f *ownerHelperFile) { f.Runtime = "codex" }),
		"runtime in other case": missing(func(f *ownerHelperFile) { f.Runtime = "Claude" }),
		"both thread variables set": func(t *testing.T, _ string) {
			writeHelperLedgerFile(t, helperLedgerFile())
			claude(t)
			t.Setenv("CODEX_THREAD_ID", helperLedgerThread)
		},
		"malformed thread variable": func(t *testing.T, _ string) {
			file := helperLedgerFile()
			file.Thread = "not-a-thread"
			writeHelperLedgerFile(t, file)
			t.Setenv("CLAUDE_CODE_SESSION_ID", file.Thread)
		},
		"file missing agent":   missing(func(f *ownerHelperFile) { f.Agent = "" }),
		"file missing run":     missing(func(f *ownerHelperFile) { f.Run = "" }),
		"file missing thread":  missing(func(f *ownerHelperFile) { f.Thread = "" }),
		"file missing runtime": missing(func(f *ownerHelperFile) { f.Runtime = "" }),
		"unsafe agent id":      missing(func(f *ownerHelperFile) { f.Agent = "../agt_00000000000000a1" }),
		"invalid task id": func(t *testing.T, relay string) {
			file := helperLedgerFile()
			file.Task = "tsk_ledger"
			writeHelperLedgerFile(t, file)
			claude(t)
		},
		"malformed JSON": func(t *testing.T, _ string) {
			if err := os.WriteFile(ownerHelperPath(helperLedgerHub, helperLedgerTask), []byte(`{"hub":"`+helperLedgerHub+`","thread":"`+helperLedgerThread+`"`), 0600); err != nil {
				t.Fatal(err)
			}
			claude(t)
		},
		"oversized file": missing(func(f *ownerHelperFile) { f.Name = strings.Repeat("n", ownerHelperFileBytes) }),
		"file under another hub's name": func(t *testing.T, relay string) {
			misnamed(t, relay, filepath.Base(ownerHelperPath("http://other.example:1", helperLedgerTask)))
		},
		"file under another task's name": func(t *testing.T, relay string) {
			misnamed(t, relay, filepath.Base(ownerHelperPath(helperLedgerHub, "tsk_00000000000000b2")))
		},
		"copied file": func(t *testing.T, relay string) { misnamed(t, relay, "copy.owner-helper.json") },
		"TAILTERM_TASK names another project": func(t *testing.T, _ string) {
			writeHelperLedgerFile(t, helperLedgerFile())
			claude(t)
			t.Setenv("TAILTERM_TASK", "tsk_00000000000000b2")
		},
		"TAILTERM_RUN names another run": func(t *testing.T, _ string) {
			writeHelperLedgerFile(t, helperLedgerFile())
			claude(t)
			t.Setenv("TAILTERM_RUN", "run_replaced")
		},
		"two files carry the same thread": func(t *testing.T, _ string) {
			writeHelperLedgerFile(t, helperLedgerFile())
			second := helperLedgerFile()
			second.Task, second.Agent, second.Run = "tsk_00000000000000b2", "agt_00000000000000b2", "run_helper_b2"
			writeHelperLedgerFile(t, second)
			claude(t)
		},
		"more helper files than one lookup reads": func(t *testing.T, _ string) {
			writeHelperLedgerFile(t, helperLedgerFile())
			for i := 0; i < ownerHelperCandidates; i++ {
				other := helperLedgerFile()
				other.Task, other.Agent, other.Thread = fmt.Sprintf("tsk_%016x", 0xc0+i), fmt.Sprintf("agt_%016x", 0xc0+i), fmt.Sprintf("33333333-3333-4333-8333-%012x", i)
				writeHelperLedgerFile(t, other)
			}
			claude(t)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			root, relay := helperLedgerSandbox(t)
			setup(t, relay)
			e := readEnv()
			if e.agent != "" {
				t.Fatalf("the sandbox environment has an agent: %q", e.agent)
			}
			for _, event := range []string{"PreToolUse", "PostToolUse"} {
				payload := toolPayload(event, map[string]any{"session_id": helperLedgerThread, "tool_use_id": "u1", "tool_name": "Bash"})
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				_, _ = w.WriteString(payload)
				w.Close()
				run := runToolHook(t, e, "tool", "", r)
				left, _ := io.ReadAll(r)
				r.Close()
				if run.err != nil || run.out != "" || string(left) != payload {
					t.Errorf("%s = %v, output %q, %d of %d stdin bytes left", event, run.err, run.out, len(left), len(payload))
				}
			}
			requireEmptyRoot(t, root)
		})
	}
}

// a5: with a helper for each of two projects on the host, only the one
// registered from this thread is written.
func TestToolLedgerHelperTwoProjects(t *testing.T) {
	root, _ := helperLedgerSandbox(t)
	first := helperLedgerFile()
	second := helperLedgerFile()
	second.Task, second.Agent, second.Run, second.Thread = "tsk_00000000000000b2", "agt_00000000000000b2", "run_helper_b2", "22222222-2222-4222-8222-222222222222"
	writeHelperLedgerFile(t, first)
	writeHelperLedgerFile(t, second)
	t.Setenv("CLAUDE_CODE_SESSION_ID", second.Thread)
	e := helperLedgerEnv(t)
	call := map[string]any{"session_id": second.Thread, "tool_use_id": "u1", "tool_name": "Read"}
	toolHook(t, e, "PreToolUse", call)
	toolHook(t, e, "PostToolUse", call)
	rows := toolLedgerRows(t, root, second.Agent)
	if len(rows) != 1 || rows[0]["agent"] != second.Agent || rows[0]["task"] != second.Task || rows[0]["run"] != second.Run || rows[0]["outcome"] != "ok" {
		t.Fatalf("rows for the matching helper = %v", rows)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != second.Agent {
		t.Fatalf("ledger root = %v, %v; want only %s", entries, err, second.Agent)
	}
}

// a6: the lookup stays inside the hook's bound in a relay directory far
// larger than a busy host's. A timing miss is retried twice, since a stalled
// test host is not the property; every attempt must still write its row.
func TestToolLedgerHelperWithinBound(t *testing.T) {
	root, relay := helperLedgerSandbox(t)
	for i := 0; i < 10000; i++ {
		name := fmt.Sprintf("%064x-agt_%016x.binding.json", i, i)
		if err := os.WriteFile(filepath.Join(relay, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	file := helperLedgerFile()
	other := helperLedgerFile()
	other.Task, other.Agent, other.Run, other.Thread = "tsk_00000000000000b2", "agt_00000000000000b2", "run_helper_b2", "22222222-2222-4222-8222-222222222222"
	writeHelperLedgerFile(t, file)
	writeHelperLedgerFile(t, other)
	t.Setenv("CLAUDE_CODE_SESSION_ID", file.Thread)
	e := helperLedgerEnv(t)
	var slowest time.Duration
	for attempt := 1; attempt <= 3; attempt++ {
		slowest = 0
		call := map[string]any{"session_id": file.Thread, "tool_use_id": fmt.Sprintf("u%d", attempt), "tool_name": "Bash"}
		for _, event := range []string{"PreToolUse", "PostToolUse"} {
			r := runToolHook(t, e, "tool", toolPayload(event, call), nil)
			if r.err != nil || r.out != "" {
				t.Fatalf("%s = %v, output %q", event, r.err, r.out)
			}
			slowest = max(slowest, r.elapsed)
		}
		if slowest < toolLedgerBound {
			if rows := toolLedgerRows(t, root, file.Agent); len(rows) == 0 || rows[len(rows)-1]["toolUseId"] != call["tool_use_id"] || rows[len(rows)-1]["outcome"] != "ok" {
				t.Fatalf("rows after an in-bound call = %v", rows)
			}
			return
		}
	}
	t.Fatalf("slowest hook took %v in each of three attempts; want under %v", slowest, toolLedgerBound)
}

// a3: a helper row holds the row fields and nothing else: no token, no
// tool input and none of the helper file's registration fields.
func TestToolLedgerHelperStoresNoSecrets(t *testing.T) {
	root, _ := helperLedgerSandbox(t)
	markers := []string{"MARK-TOKEN", "MARK-INPUT", "MARK-REQUEST-ID", "MARK-REQUEST-HASH", "MARK-REGISTRATION"}
	file := helperLedgerFile()
	file.RequestID, file.RequestHash, file.Registration = markers[2], markers[3], markers[4]
	writeHelperLedgerFile(t, file)
	t.Setenv("TAILTERM_TOKEN", markers[0])
	t.Setenv("CLAUDE_CODE_SESSION_ID", file.Thread)
	e := helperLedgerEnv(t)
	if e.token != markers[0] {
		t.Fatalf("the test token did not reach the environment: %q", e.token)
	}
	fields := func(id string) map[string]any {
		return map[string]any{"session_id": file.Thread, "tool_use_id": id, "tool_name": "Bash", "duration_ms": 3,
			"tool_input": map[string]any{"command": "echo MARK-INPUT", "env": map[string]any{"TOKEN": "MARK-INPUT"}}}
	}
	toolHook(t, e, "PreToolUse", fields("u1"))
	toolHook(t, e, "PostToolUse", fields("u1"))
	toolHook(t, e, "PreToolUse", fields("u2"))
	toolHook(t, e, "PostToolUseFailure", fields("u2"))
	toolHook(t, e, "PreToolUse", fields("u3")) // stays pending
	if r := runToolHook(t, e, "tool", `{"tool_input":"MARK-INPUT"`, nil); r.err != nil || r.out != "" {
		t.Fatalf("malformed input = %v, output %q", r.err, r.out)
	}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for _, marker := range markers {
			if bytes.Contains(data, []byte(marker)) || strings.Contains(path, marker) {
				t.Errorf("%s holds %s", path, marker)
			}
		}
		return nil
	})
	if err != nil || files != 3 { // ledger, lock, one pending
		t.Fatalf("read %d files, %v", files, err)
	}
	rows := toolLedgerRows(t, root, file.Agent)
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	const complete = "agent,argsDigest,durationMs,durationSource,outcome,run,session,task,time,tool,toolUseId,v"
	for i, want := range []string{complete, complete, "agent,argsDigest,outcome,run,session,task,time,tool,toolUseId,v"} {
		if rowKeys(rows[i]) != want {
			t.Errorf("row %d keys = %s; want %s", i, rowKeys(rows[i]), want)
		}
		if rows[i]["agent"] != file.Agent || rows[i]["task"] != file.Task || rows[i]["run"] != file.Run {
			t.Errorf("row %d identity = %v %v %v", i, rows[i]["agent"], rows[i]["task"], rows[i]["run"])
		}
	}
}

// Secret redaction off, which is the default and every unusable setting:
// a PostToolUse whose output holds a token-shaped value prints nothing and
// writes exactly the row the ledger wrote before redaction existed.
func TestToolLedgerRedactionOffChangesNothing(t *testing.T) {
	c := redactShapeCases[0]
	value := c.make(t, c.n)
	settings := map[string]string{
		"no file": "", "off": `{"claudeSecretRedaction":"off"}`, "another value": `{"claudeSecretRedaction":"yes"}`,
		"another case": `{"claudesecretredaction":"redact"}`, "malformed": `{"claudeSecretRedaction":"redact"`, "no key": `{"claudeStallAction":"report"}`,
		"a directory": "/",
	}
	for label, content := range settings {
		t.Run(label, func(t *testing.T) {
			e, root := toolLedgerSandbox(t)
			path := os.Getenv("TAILTERM_REDACT_SETTING_FILE")
			switch content {
			case "":
			case "/":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			e.token = value
			input := map[string]any{"command": "cat value.txt"}
			toolHook(t, e, "PostToolUse", map[string]any{"session_id": "s1", "tool_name": "Bash", "tool_use_id": "u1", "tool_input": input, "duration_ms": 12,
				"tool_response": map[string]any{"stdout": value + "\n", "stderr": "", "interrupted": false}})
			rows := toolLedgerRows(t, root, e.agent)
			canonical, _ := json.Marshal(input)
			want := map[string]any{"v": float64(1), "time": rows[0]["time"], "task": "tsk_ledger", "agent": "agt_ledger", "run": "run_ledger", "session": "s1", "tool": "Bash", "toolUseId": "u1",
				"argsDigest": toolArgsDigest(canonical), "outcome": "ok", "durationMs": float64(12), "durationSource": "claude"}
			// fmt prints a map's keys in sorted order.
			if len(rows) != 1 || fmt.Sprint(rows[0]) != fmt.Sprint(want) {
				t.Fatalf("the row with redaction off is not the ledger's own row: %d rows", len(rows))
			}
		})
	}
}
