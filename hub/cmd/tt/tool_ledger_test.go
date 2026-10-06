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
// synthetic identity. No test here needs a hub.
func toolLedgerSandbox(t *testing.T) (env, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tool-ledger")
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
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

// a13: only tool goes through the registry; the four existing hooks, codex
// and unknown names take the code that was there before.
func TestHookRegistryHoldsOnlyTool(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	if len(hookHandlers) != 1 || hookHandlers["tool"] == nil {
		t.Fatalf("registry = %v; want only tool", hookHandlers)
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
func TestToolLedgerNeverBlocks(t *testing.T) {
	pre := toolPayload("PreToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}})
	post := toolPayload("PostToolUse", map[string]any{"session_id": "s1", "tool_use_id": "u1", "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}})
	type result struct {
		run  toolHookRun
		rows []map[string]any
	}
	cases := []struct {
		name  string
		run   func(t *testing.T, e env, root string) toolHookRun
		check func(t *testing.T, rows []map[string]any)
	}{
		{"stdin held open", func(t *testing.T, e env, root string) toolHookRun {
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
		{"write blocks", func(t *testing.T, e env, root string) toolHookRun {
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
		{"write panics", func(t *testing.T, e env, root string) toolHookRun {
			setToolLedger(t, &toolLedgerWrite, func(*os.File, []byte) error { panic("disk on fire") })
			return runToolHook(t, e, "tool", post, nil)
		}, nil},
		{"unwritable ledger directory", func(t *testing.T, e env, root string) toolHookRun {
			if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			return runToolHook(t, e, "tool", post, nil)
		}, nil},
		{"lock held elsewhere", func(t *testing.T, e env, root string) toolHookRun {
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
		{"empty input", func(t *testing.T, e env, root string) toolHookRun {
			return runToolHook(t, e, "tool", "", nil)
		}, func(t *testing.T, rows []map[string]any) {
			if len(rows) != 1 || rows[0]["outcome"] != "unreadable" || rows[0]["oversize"] != nil {
				t.Fatalf("rows = %v", rows)
			}
		}},
		{"malformed input", func(t *testing.T, e env, root string) toolHookRun {
			return runToolHook(t, e, "tool", `{"hook_event_name":"PreToolUse","tool_name":`, nil)
		}, func(t *testing.T, rows []map[string]any) {
			if len(rows) != 1 || rows[0]["outcome"] != "unreadable" || rows[0]["tool"] != "" {
				t.Fatalf("rows = %v", rows)
			}
		}},
		{"9 MiB input", func(t *testing.T, e env, root string) toolHookRun {
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
				if last.run.elapsed < toolLedgerBound {
					break
				}
			}
			if last.run.elapsed >= toolLedgerBound {
				t.Fatalf("hook took %v three times; want under %v", last.run.elapsed, toolLedgerBound)
			}
			if c.check != nil {
				c.check(t, last.rows)
			}
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
