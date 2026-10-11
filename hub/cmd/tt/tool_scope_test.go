package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Every path in this file is under a test's temp directory or is a string
// handed to the hook as tool input. No file a hook input names is written.

// scopeAssign is a typed ASSIGN addressed to e's agent.
func scopeAssign(e env, seq int64, owns ...string) api.Message {
	return api.Message{Seq: seq, To: e.agent, Text: "ASSIGN: MARK-TEXT",
		WorkItems: []api.MessageWorkItem{{ItemID: "wi_related", ItemRevision: 9, Relationship: "related"}, {ItemID: "wi_scope", ItemRevision: 3, Relationship: "primary"}},
		Envelope: &api.Envelope{Kind: api.EnvelopeKindAssign, Subject: "MARK-SUBJECT",
			Body: api.EnvelopeBody{Objective: "MARK-OBJECTIVE", Owns: owns, Acceptance: map[string]string{"a1": "MARK-ACCEPTANCE"}}}}
}

// scopeRepo is an empty repository root: a directory holding .git.
func scopeRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func scopeTouch(t *testing.T, root, rel string, dir bool) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if dir {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

// scopePre is the hook input of one edit call. NotebookEdit names its file
// in notebook_path; the other three in file_path.
func scopePre(tool, id, cwd, file string) map[string]any {
	input := map[string]any{"file_path": file, "old_string": "MARK-INPUT", "new_string": "MARK-INPUT", "content": "MARK-INPUT"}
	if tool == "NotebookEdit" {
		input = map[string]any{"notebook_path": file, "new_source": "MARK-INPUT"}
	}
	return map[string]any{"session_id": "s1", "tool_use_id": id, "tool_name": tool, "tool_input": input, "cwd": cwd}
}

func wouldReportRows(rows []map[string]any) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		if row["outcome"] == toolScopeOutcome {
			out = append(out, row)
		}
	}
	return out
}

func scopeReadCache(t *testing.T, root, agent string) (toolScopeCache, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, agent, toolScopeCacheName))
	if errors.Is(err, os.ErrNotExist) {
		return toolScopeCache{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var cache toolScopeCache
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return cache, true
}

// scopeHoldLock takes an exclusive lock on a file in the agent's directory,
// as another process would, and returns its release.
func scopeHoldLock(t *testing.T, root, agent, name string) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, agent), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, agent, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }) }
	t.Cleanup(release)
	return release
}

// scopeInboxHub is an inbox hub whose handler agent has a typed ASSIGN, a
// typed ASSIGN to the lead, a typed request and a text-convention ASSIGN.
type scopeInboxHub struct {
	inboxHub
	item                       api.WorkItem
	assign, other, plain, text api.Message
}

func newScopeInboxHub(t *testing.T) scopeInboxHub {
	t.Helper()
	h := scopeInboxHub{inboxHub: newInboxHub(t)}
	ctx := context.Background()
	var err error
	if h.item, err = h.c.CreateWorkItem(ctx, h.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "scope fixture bug", RequestID: "scope-item"}); err != nil {
		t.Fatal(err)
	}
	link := []api.MessageWorkItem{{ItemTaskID: h.task.ID, ItemID: h.item.ID, ItemRevision: h.item.Revision, Relationship: "primary"}}
	post := func(req api.PostMessageRequest) api.Message {
		t.Helper()
		m, err := h.st.PostMessage(ctx, h.task.ID, req, h.by)
		if err != nil {
			t.Fatalf("post to %s: %v", req.To, err)
		}
		return m
	}
	body := api.EnvelopeBody{Objective: "MARK-OBJECTIVE", Owns: []string{"hub/cmd/tt/a.go", "docs"}, Acceptance: map[string]string{"a1": "MARK-ACCEPTANCE"}}
	h.plain = post(api.PostMessageRequest{AgentID: h.lead.ID, To: h.handler.ID, Envelope: &api.Envelope{Kind: "request", To: "database", Subject: "A request without owned paths", Body: api.EnvelopeBody{Ask: "Nothing to cache"}}})
	h.text = post(api.PostMessageRequest{AgentID: h.lead.ID, To: h.handler.ID, Text: "ASSIGN: text convention\nOwns: hub/cmd/tt/text.go"})
	h.other = post(api.PostMessageRequest{AgentID: h.handler.ID, To: h.lead.ID, Envelope: &api.Envelope{Kind: "assign", To: "lead", Subject: "An assignment for the lead", Body: body}})
	h.assign = post(api.PostMessageRequest{AgentID: h.lead.ID, RunID: h.lead.RunID, To: h.handler.ID, RequestID: "scope-assign", WorkItems: link, Envelope: &api.Envelope{Kind: "assign", To: "database", Subject: "MARK-SUBJECT for the handler", Body: body}})
	return h
}

// scopeLedgerDir points the ledger at a fresh temp directory.
func scopeLedgerDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tool-ledger")
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
	return root
}

func scopeDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// a1: every list form of tt inbox that fetches a typed ASSIGN addressed to
// this agent writes the private cache, and nothing else does.
func TestToolScopeCacheWrite(t *testing.T) {
	h := newScopeInboxHub(t)
	e := h.e
	for _, args := range [][]string{{"--unread"}, {"--unread", "--json"}, {"--unread", "--mark-read"}, {}, {"--json"}, {"--board"}, {"--after", fmt.Sprint(h.assign.Seq - 1)}} {
		t.Run("tt inbox "+strings.Join(args, " "), func(t *testing.T) {
			root := scopeLedgerDir(t)
			if _, err := captureCLIOutput(t, func() error { return cmdInbox(e, args) }); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, e.agent)
			data, err := os.ReadFile(filepath.Join(dir, toolScopeCacheName))
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(data, &keys); err != nil {
				t.Fatal(err)
			}
			if got := rowKeysRaw(keys); got != "assignSeq,item,itemRevision,owns,run,time,v" {
				t.Fatalf("cache keys = %s", got)
			}
			cache, _ := scopeReadCache(t, root, e.agent)
			stamp, err := time.Parse(time.RFC3339, cache.Time)
			if err != nil || stamp.Location() != time.UTC {
				t.Fatalf("time = %q, %v; want UTC RFC 3339", cache.Time, err)
			}
			cache.Time = ""
			want := toolScopeCache{V: 1, Run: e.runID, AssignSeq: h.assign.Seq, Item: h.item.ID, ItemRevision: h.item.Revision, Owns: []string{"hub/cmd/tt/a.go", "docs"}}
			if !reflect.DeepEqual(cache, want) {
				t.Fatalf("cache = %+v; want %+v", cache, want)
			}
			for _, marker := range []string{"MARK-", "handler", "Objective", "objective", "subject", "acceptance"} {
				if strings.Contains(string(data), marker) {
					t.Errorf("cache holds %q: %s", marker, data)
				}
			}
			for name, mode := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, toolScopeCacheName): 0600, filepath.Join(dir, "owns.lock"): 0600} {
				if info, err := os.Stat(name); err != nil || info.Mode().Perm() != mode {
					t.Errorf("%s mode = %v, %v; want %v", name, info.Mode().Perm(), err, mode)
				}
			}
			if names := strings.Join(scopeDirNames(t, dir), ","); names != "owns.json,owns.lock" {
				t.Fatalf("directory holds %s; want the cache and its lock only", names)
			}
		})
	}
	t.Run("nothing to cache", func(t *testing.T) {
		root := scopeLedgerDir(t)
		// The lead's inbox holds only the ASSIGN the handler sent it; read as
		// the handler, the Board shows that ASSIGN addressed to someone else.
		noAgent := env{hub: e.hub, task: e.task}
		stranger := env{hub: e.hub, task: e.task, agent: "agt_stranger", runID: "run_stranger"}
		badName := env{hub: e.hub, task: e.task, agent: "../agt_escape", runID: "run_escape"}
		for _, who := range []env{noAgent, stranger, badName} {
			for _, args := range [][]string{{"--board"}, {"--board", "--json"}} {
				if _, err := captureCLIOutput(t, func() error { return cmdInbox(who, args) }); err != nil {
					t.Fatal(err)
				}
			}
		}
		before := fmt.Sprint(h.assign.Seq)
		for _, args := range [][]string{{"--before", before}, {"--before", before, "--json"}, {"--board", "--before", before}} {
			if _, err := captureCLIOutput(t, func() error { return cmdInbox(e, args) }); err != nil {
				t.Fatal(err)
			}
		}
		toolScopeRemember(e, []api.Message{h.other, h.plain, h.text})
		toolScopeRemember(e, nil)
		if names := scopeDirNames(t, filepath.Dir(root)); len(names) != 0 {
			t.Fatalf("an inbox read with no ASSIGN for the agent wrote %v", names)
		}
	})
}

func rowKeysRaw(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// a1: tt inbox --seq N caches an ASSIGN addressed to this agent and prints
// exactly what it printed before; one addressed to someone else is not cached.
func TestToolScopeCacheSeq(t *testing.T) {
	h := newScopeInboxHub(t)
	e := h.e
	for _, flags := range [][]string{nil, {"--json"}} {
		root := scopeLedgerDir(t)
		seq := fmt.Sprint(h.assign.Seq)
		want, err := captureCLIOutput(t, func() error { return printInboxMessage(h.c, e.task, h.assign.Seq, flags != nil) })
		if err != nil {
			t.Fatal(err)
		}
		got, err := captureCLIOutput(t, func() error { return cmdInbox(e, append([]string{"--seq", seq}, flags...)) })
		if err != nil || got != want || !strings.Contains(got, "MARK-SUBJECT") {
			t.Fatalf("--seq %v printed %q, %v; want %q", flags, got, err, want)
		}
		if cache, ok := scopeReadCache(t, root, e.agent); !ok || cache.AssignSeq != h.assign.Seq || len(cache.Owns) != 2 || cache.Item != h.item.ID {
			t.Fatalf("--seq %v cache = %+v, %v", flags, cache, ok)
		}
		root = scopeLedgerDir(t)
		for _, m := range []api.Message{h.other, h.plain, h.text} {
			want, _ := captureCLIOutput(t, func() error { return printInboxMessage(h.c, e.task, m.Seq, flags != nil) })
			got, err := captureCLIOutput(t, func() error { return cmdInbox(e, append([]string{"--seq", fmt.Sprint(m.Seq)}, flags...)) })
			if err != nil || got != want {
				t.Fatalf("--seq %d %v printed %q, %v; want %q", m.Seq, flags, got, err, want)
			}
		}
		missing := h.assign.Seq + 1000
		if _, err := captureCLIOutput(t, func() error { return cmdInbox(e, []string{"--seq", fmt.Sprint(missing)}) }); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("no message #%d", missing)) {
			t.Fatalf("missing --seq error = %v", err)
		}
		// Without an agent identity the extra fetch is not made at all.
		if _, err := captureCLIOutput(t, func() error { return cmdInbox(env{hub: e.hub, task: e.task}, []string{"--seq", seq}) }); err != nil {
			t.Fatal(err)
		}
		if names := scopeDirNames(t, filepath.Dir(root)); len(names) != 0 {
			t.Fatalf("--seq of a message that is no ASSIGN for the agent wrote %v", names)
		}
	}
}

// a2: the newest ASSIGN of a run stays cached.
func TestToolScopeCacheLatestWins(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	seq := func() int64 {
		t.Helper()
		cache, ok := scopeReadCache(t, root, e.agent)
		if !ok {
			t.Fatal("no cache")
		}
		return cache.AssignSeq
	}
	toolScopeRemember(e, []api.Message{scopeAssign(e, 7, "a"), scopeAssign(e, 9, "b"), scopeAssign(e, 8, "c")})
	if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != 9 || !reflect.DeepEqual(cache.Owns, []string{"b"}) || cache.Item != "wi_scope" || cache.ItemRevision != 3 {
		t.Fatalf("highest of one fetch: %+v", cache)
	}
	for _, lower := range []int64{9, 8, 1} {
		toolScopeRemember(e, []api.Message{scopeAssign(e, lower, "older")})
		if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != 9 || cache.Owns[0] != "b" {
			t.Fatalf("seq %d replaced the cache: %+v", lower, cache)
		}
	}
	toolScopeRemember(e, []api.Message{scopeAssign(e, 12, "amended")})
	if cache, _ := scopeReadCache(t, root, e.agent); seq() != 12 || cache.Owns[0] != "amended" {
		t.Fatalf("a higher seq did not replace the cache: %+v", cache)
	}
	// A cache that cannot be read is no cache: any ASSIGN replaces it.
	if err := os.WriteFile(filepath.Join(root, e.agent, toolScopeCacheName), []byte(`{"v":1,"assignSeq":99`), 0600); err != nil {
		t.Fatal(err)
	}
	toolScopeRemember(e, []api.Message{scopeAssign(e, 3, "after a torn file")})
	if seq() != 3 {
		t.Fatalf("seq after an unreadable cache = %d; want 3", seq())
	}
	// Links: the first link stands in when none is primary, and none is empty.
	first := scopeAssign(e, 20, "x")
	first.WorkItems[1].Relationship = "related"
	toolScopeRemember(e, []api.Message{first})
	if cache, _ := scopeReadCache(t, root, e.agent); cache.Item != "wi_related" || cache.ItemRevision != 9 {
		t.Fatalf("first link: %+v", cache)
	}
	none := scopeAssign(e, 21, "x")
	none.WorkItems = nil
	toolScopeRemember(e, []api.Message{none})
	if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != 21 || cache.Item != "" || cache.ItemRevision != 0 {
		t.Fatalf("no link: %+v", cache)
	}
}

// a2: an ASSIGN past either bound keeps no paths.
func TestToolScopeCacheBound(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	many := func(n int) []string {
		owns := make([]string, n)
		for i := range owns {
			owns[i] = fmt.Sprintf("dir/file%03d.go", i)
		}
		return owns
	}
	for i, c := range []struct {
		name     string
		owns     []string
		overflow bool
	}{
		{"256 entries", many(toolScopeMaxEntries), false},
		{"257 entries", many(toolScopeMaxEntries + 1), true},
		{"one entry of 512 bytes", []string{"a", strings.Repeat("p", toolScopeEntryBytes)}, false},
		{"one entry of 513 bytes", []string{"a", strings.Repeat("p", toolScopeEntryBytes+1)}, true},
		{"entries that escape past the file bound", func() []string {
			owns := make([]string, toolScopeMaxEntries)
			for i := range owns {
				owns[i] = strings.Repeat("<", toolScopeEntryBytes)
			}
			return owns
		}(), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			toolScopeRemember(e, []api.Message{scopeAssign(e, int64(10+i), c.owns...)})
			data, err := os.ReadFile(filepath.Join(root, e.agent, toolScopeCacheName))
			if err != nil || len(data) > toolScopeCacheBytes {
				t.Fatalf("cache of %d bytes, %v", len(data), err)
			}
			cache, _ := scopeReadCache(t, root, e.agent)
			if cache.AssignSeq != int64(10+i) || cache.Overflow != c.overflow {
				t.Fatalf("cache seq %d overflow %v; want seq %d overflow %v", cache.AssignSeq, cache.Overflow, 10+i, c.overflow)
			}
			if c.overflow && (cache.Owns != nil || strings.Contains(string(data), `"owns"`)) {
				t.Fatalf("an overflow cache holds paths: %.80s", data)
			}
			if !c.overflow && !reflect.DeepEqual(cache.Owns, c.owns) {
				t.Fatalf("cache lost entries: %d of %d", len(cache.Owns), len(c.owns))
			}
			if loaded, ok := toolScopeLoad(filepath.Join(root, e.agent)); !ok || loaded.AssignSeq != cache.AssignSeq {
				t.Fatalf("the hook's reader refuses the cache the writer wrote: %v", ok)
			}
		})
	}
}

// a2: a recovery run of the same agent re-caches the ASSIGN it reads, even
// one older than the previous run's.
func TestToolScopeCacheNewRun(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	toolScopeRemember(e, []api.Message{scopeAssign(e, 40, "amended")})
	next := e
	next.runID = "run_recovery"
	toolScopeRemember(next, []api.Message{scopeAssign(e, 31, "original")})
	cache, _ := scopeReadCache(t, root, e.agent)
	if cache.Run != "run_recovery" || cache.AssignSeq != 31 || cache.Owns[0] != "original" {
		t.Fatalf("cache after the new run read an older ASSIGN: %+v", cache)
	}
	toolScopeRemember(next, []api.Message{scopeAssign(e, 30, "older still")})
	if cache, _ = scopeReadCache(t, root, e.agent); cache.AssignSeq != 31 {
		t.Fatalf("a lower seq of the same new run replaced the cache: %+v", cache)
	}
	// The previous run's hook no longer uses it.
	repo := scopeRepo(t)
	toolHook(t, e, "PreToolUse", scopePre("Edit", "u1", repo, filepath.Join(repo, "outside.go")))
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != 0 {
		t.Fatalf("a hook of another run used the cache: %v", rows)
	}
	toolHook(t, next, "PreToolUse", scopePre("Edit", "u2", repo, filepath.Join(repo, "outside.go")))
	if rows := wouldReportRows(toolLedgerRows(t, root, e.agent)); len(rows) != 1 || rows[0]["run"] != "run_recovery" {
		t.Fatalf("the new run's hook wrote %v", rows)
	}
}

// a2: the read, the comparison and the rename are one step under the lock.
// The first writer is stopped at a seam, not by a sleep, so the second can
// only ever be waiting while the first holds the lock.
func TestToolScopeCacheInterleaving(t *testing.T) {
	for _, c := range []struct {
		name          string
		first, second int64
	}{{"older writer paused, newer waits", 11, 12}, {"newer writer paused, older waits", 12, 11}} {
		t.Run(c.name, func(t *testing.T) {
			e, root := toolLedgerSandbox(t)
			toolScopeRemember(e, []api.Message{scopeAssign(e, 10, "ten")})
			setToolLedger(t, &toolScopeLockWait, time.Minute)
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			setToolLedger(t, &toolScopeHold, func() {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
			})
			done := make(chan int64, 2)
			write := func(seq int64) {
				toolScopeRemember(e, []api.Message{scopeAssign(e, seq, fmt.Sprintf("seq-%d", seq))})
				done <- seq
			}
			go write(c.first)
			<-entered // the first writer has read seq 10 and holds the lock
			go write(c.second)
			select {
			case seq := <-done:
				t.Fatalf("writer of seq %d finished while seq %d held the lock", seq, c.first)
			case <-time.After(50 * time.Millisecond):
			}
			if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != 10 {
				t.Fatalf("cache changed before the paused writer renamed: %+v", cache)
			}
			close(release)
			for i := 0; i < 2; i++ {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("a writer never returned")
				}
			}
			if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != 12 || cache.Owns[0] != "seq-12" {
				t.Fatalf("cache ended at %+v; want seq 12", cache)
			}
		})
	}
	t.Run("16 writers", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		setToolLedger(t, &toolScopeLockWait, time.Minute)
		for round := 0; round < 5; round++ {
			seqs := rand.Perm(16)
			var wg sync.WaitGroup
			for _, n := range seqs {
				wg.Add(1)
				go func(seq int64) {
					defer wg.Done()
					toolScopeRemember(e, []api.Message{scopeAssign(e, seq, fmt.Sprintf("seq-%d", seq))})
				}(int64(100*round + n + 1))
			}
			wg.Wait()
			if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != int64(100*round+16) || cache.Owns[0] != fmt.Sprintf("seq-%d", 100*round+16) {
				t.Fatalf("round %d ended at %+v; want seq %d", round, cache, 100*round+16)
			}
		}
		if names := strings.Join(scopeDirNames(t, filepath.Join(root, e.agent)), ","); names != "owns.json,owns.lock" {
			t.Fatalf("writers left %s", names)
		}
	})
	t.Run("no lock, no write", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		toolScopeRemember(e, []api.Message{scopeAssign(e, 10, "ten")})
		setToolLedger(t, &toolScopeLockWait, 30*time.Millisecond)
		release := scopeHoldLock(t, root, e.agent, "owns.lock")
		start := time.Now()
		toolScopeRemember(e, []api.Message{scopeAssign(e, 11, "eleven")})
		if elapsed := time.Since(start); elapsed < 30*time.Millisecond || elapsed > 5*time.Second {
			t.Fatalf("writer returned after %v; want the 30 ms wait", elapsed)
		}
		if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != 10 {
			t.Fatalf("a writer without the lock wrote %+v", cache)
		}
		release()
		toolScopeRemember(e, []api.Message{scopeAssign(e, 11, "eleven")})
		if cache, _ := scopeReadCache(t, root, e.agent); cache.AssignSeq != 11 {
			t.Fatalf("the next read did not cache the ASSIGN: %+v", cache)
		}
		if names := strings.Join(scopeDirNames(t, filepath.Join(root, e.agent)), ","); names != "owns.json,owns.lock" {
			t.Fatalf("directory holds %s", names)
		}
	})
}

// a3: tt inbox prints and returns the same whether or not the cache can be
// written, and a held cache lock delays it by no more than the lock wait.
func TestToolScopeCacheNeverFailsInbox(t *testing.T) {
	h := newScopeInboxHub(t)
	e := h.e
	forms := [][]string{{"--unread"}, {"--unread", "--json"}, {}, {"--json"}, {"--seq", fmt.Sprint(h.assign.Seq)}, {"--seq", fmt.Sprint(h.assign.Seq + 1000)}}
	type result struct{ out, err string }
	run := func(args []string) (result, time.Duration) {
		t.Helper()
		start := time.Now()
		out, err := captureCLIOutput(t, func() error { return cmdInbox(e, args) })
		elapsed := time.Since(start)
		return result{out, fmt.Sprint(err)}, elapsed
	}
	root := scopeLedgerDir(t)
	want := map[string]result{}
	for _, args := range forms {
		want[strings.Join(args, " ")], _ = run(args)
	}
	if _, ok := scopeReadCache(t, root, e.agent); !ok {
		t.Fatal("the writable run cached nothing, so the comparison below proves nothing")
	}
	t.Run("unwritable ledger folder", func(t *testing.T) {
		// A regular file where the ledger root should be: no directory can be
		// made under it, whoever runs the test.
		blocker := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(blocker, nil, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TAILTERM_TOOL_LEDGER_DIR", filepath.Join(blocker, "tool-ledger"))
		for _, args := range forms {
			if got, _ := run(args); got != want[strings.Join(args, " ")] {
				t.Errorf("tt inbox %v = %+v; want %+v", args, got, want[strings.Join(args, " ")])
			}
		}
		// A read-only agent directory: the temp file cannot be created.
		root := scopeLedgerDir(t)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, e.agent), 0500); err != nil {
			t.Fatal(err)
		}
		for _, args := range forms {
			if got, _ := run(args); got != want[strings.Join(args, " ")] {
				t.Errorf("read-only directory: tt inbox %v = %+v; want %+v", args, got, want[strings.Join(args, " ")])
			}
		}
		if os.Getuid() != 0 {
			if names := scopeDirNames(t, filepath.Join(root, e.agent)); len(names) != 0 {
				t.Errorf("a read-only directory gained %v", names)
			}
		}
	})
	t.Run("held lock", func(t *testing.T) {
		root := scopeLedgerDir(t)
		scopeHoldLock(t, root, e.agent, "owns.lock")
		for _, args := range forms[:5] {
			var added time.Duration
			for attempt := 0; attempt < 3; attempt++ {
				t.Setenv("TAILTERM_TOOL_LEDGER_DIR", filepath.Join(t.TempDir(), "free"))
				_, free := run(args)
				t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
				got, held := run(args)
				if got != want[strings.Join(args, " ")] {
					t.Fatalf("tt inbox %v under a held lock = %+v; want %+v", args, got, want[strings.Join(args, " ")])
				}
				if added = held - free; added < toolScopeLockWait+300*time.Millisecond {
					break
				}
			}
			if added >= toolScopeLockWait+300*time.Millisecond {
				t.Fatalf("a held lock delayed tt inbox %v by %v three times; want under %v", args, added, toolScopeLockWait+300*time.Millisecond)
			}
			if added < toolScopeLockWait/2 {
				t.Fatalf("tt inbox %v was delayed %v; the writer did not wait for the lock, so the test proves nothing", args, added)
			}
		}
		if _, ok := scopeReadCache(t, root, e.agent); ok {
			t.Fatal("a writer without the lock wrote the cache")
		}
	})
}

// a3: the extra fetch of a --seq read is for the cache only. With a hub that
// never answers it, tt inbox --seq prints and returns the same, about one
// second later, and caches nothing.
func TestToolScopeCacheSeqStalledFetch(t *testing.T) {
	h := newScopeInboxHub(t)
	real, err := url.Parse(h.e.hub)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(real)
	release := make(chan struct{})
	var stalled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the cache fetch names a recipient on a one-message page.
		if q := r.URL.Query(); q.Get("to") != "" && q.Get("limit") == "1" {
			stalled.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	// TestMain raised the floor under every command deadline; this test is
	// about the shipped one-second bound.
	previous := minCommandTimeout
	minCommandTimeout = productionMinCommandTimeout
	t.Cleanup(func() { minCommandTimeout = previous })
	root := scopeLedgerDir(t)
	args := []string{"--seq", fmt.Sprint(h.assign.Seq)}
	want, err := captureCLIOutput(t, func() error { return cmdInbox(env{hub: h.e.hub, task: h.e.task}, args) })
	if err != nil {
		t.Fatal(err)
	}
	e := h.e
	e.hub = srv.URL
	start := time.Now()
	got, err := captureCLIOutput(t, func() error { return cmdInbox(e, args) })
	elapsed := time.Since(start)
	if err != nil || got != want {
		t.Fatalf("--seq with a stalled cache fetch printed %q, %v; want %q", got, err, want)
	}
	if stalled.Load() != 1 {
		t.Fatalf("the cache fetch was stalled %d times; want once", stalled.Load())
	}
	if elapsed < 900*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("--seq returned after %v; want about the one-second bound of the cache fetch", elapsed)
	}
	if names := scopeDirNames(t, filepath.Dir(root)); len(names) != 0 {
		t.Fatalf("a failed cache fetch wrote %v", names)
	}
}

// a4: each edit tool outside the owned paths leaves exactly one row.
func TestToolScopeRow(t *testing.T) {
	long := strings.Repeat("é", 150) + ".go" // 303 bytes; the cut falls inside a character
	for _, c := range []struct {
		name, tool   string
		gitFile      bool
		cwd, file    string // cwd relative to the repository; file relative to cwd when not absolute
		absolute     bool
		wantPath     string
		owns         []string
		existingDirs []string
	}{
		{name: "Edit, absolute path", tool: "Edit", file: "src/outside.go", absolute: true, wantPath: "src/outside.go"},
		{name: "Write, .git is a file", tool: "Write", gitFile: true, file: "new/dir/outside.md", absolute: true, wantPath: "new/dir/outside.md"},
		{name: "MultiEdit, cwd in a subdirectory", tool: "MultiEdit", cwd: "hub/cmd", file: "hub/internal/x.go", absolute: true, wantPath: "hub/internal/x.go", existingDirs: []string{"hub/cmd"}},
		{name: "NotebookEdit, notebook_path", tool: "NotebookEdit", file: "notebooks/a.ipynb", absolute: true, wantPath: "notebooks/a.ipynb"},
		{name: "Edit, relative path from a subdirectory", tool: "Edit", cwd: "hub/cmd", file: "../internal/./x.go", wantPath: "hub/internal/x.go", existingDirs: []string{"hub/cmd"}},
		{name: "Write, relative path", tool: "Write", file: "outside.go", wantPath: "outside.go"},
		{name: "Edit, a sibling of an owned file", tool: "Edit", file: "owned/file.go.bak", absolute: true, wantPath: "owned/file.go.bak"},
		{name: "Edit, a path over 200 bytes", tool: "Edit", file: long, absolute: true, wantPath: strings.Repeat("é", 100)},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, root := toolLedgerSandbox(t)
			repo := scopeRepo(t)
			if c.gitFile {
				if err := os.Remove(filepath.Join(repo, ".git")); err != nil {
					t.Fatal(err)
				}
				scopeTouch(t, repo, ".git", false)
			}
			for _, dir := range c.existingDirs {
				scopeTouch(t, repo, dir, true)
			}
			toolScopeRemember(e, []api.Message{scopeAssign(e, 31979, "owned/file.go", "owned/dir")})
			cwd := filepath.Join(repo, filepath.FromSlash(c.cwd))
			file := c.file
			if c.absolute {
				file = filepath.Join(repo, filepath.FromSlash(c.file))
			}
			pre := scopePre(c.tool, "u1", cwd, file)
			toolHook(t, e, "PreToolUse", pre)
			rows := toolLedgerRows(t, root, e.agent)
			if len(rows) != 1 {
				t.Fatalf("rows = %v; want exactly one", rows)
			}
			row := rows[0]
			digest := toolArgsDigest(json.RawMessage(mustJSON(t, pre["tool_input"])))
			want := map[string]any{"v": 1.0, "task": e.task, "agent": e.agent, "run": e.runID, "session": "s1", "tool": c.tool, "toolUseId": "u1",
				"argsDigest": digest, "outcome": "would_report", "reportRule": "outside-owned-paths", "reportPath": c.wantPath, "assignSeq": 31979.0}
			stamp, _ := row["time"].(string)
			if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
				t.Fatalf("time = %q", stamp)
			}
			delete(row, "time")
			if !reflect.DeepEqual(row, want) {
				t.Fatalf("row = %v\nwant  %v", row, want)
			}
			path := row["reportPath"].(string)
			if path == "" || len(path) > toolScopePathBytes || !utf8.ValidString(path) || strings.Contains(path, repo) {
				t.Fatalf("reportPath = %q", path)
			}
			// The call still gets its ordinary row when it ends.
			toolHook(t, e, "PostToolUse", pre)
			if rows = toolLedgerRows(t, root, e.agent); len(rows) != 2 || rows[1]["outcome"] != "ok" || rows[1]["reportRule"] != nil || rows[1]["toolUseId"] != "u1" {
				t.Fatalf("rows after the post = %v", rows)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// a5: owned paths, paths outside the repository and other tools write no
// row, through the hook and on the real file system.
func TestToolScopeOwnedWritesNothing(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	repo := scopeRepo(t)
	outside := t.TempDir()
	scopeTouch(t, repo, "hub/cmd/tt", true)
	scopeTouch(t, repo, "[id].go", false)
	scopeTouch(t, repo, "src (archive)", true)
	scopeTouch(t, repo, "docs (new)", true)
	toolScopeRemember(e, []api.Message{scopeAssign(e, 5,
		"hub/cmd/tt/tool_scope.go", "docs/", "./scripts/run.mjs", " client/app.js ", "hub/cmd/tt/tool_scope_test.go (new)",
		filepath.Join(repo, "abs/file.go"), filepath.Join(outside, "elsewhere"), "web/*.css", "[id].go", "src (archive)", "docs (new)", "../escape", "   ")})
	n := 0
	call := func(tool, cwd, file string) int {
		t.Helper()
		n++
		toolHook(t, e, "PreToolUse", scopePre(tool, fmt.Sprintf("u%d", n), cwd, file))
		return len(wouldReportRows(toolLedgerRows(t, root, e.agent)))
	}
	in := func(rel string) string { return filepath.Join(repo, filepath.FromSlash(rel)) }
	for _, c := range []struct{ name, tool, cwd, file string }{
		{"equal to an entry", "Edit", repo, in("hub/cmd/tt/tool_scope.go")},
		{"under a directory entry with a trailing slash", "Write", repo, in("docs/deep/er/a.md")},
		{"the directory entry itself", "Edit", repo, in("docs")},
		{"entry written with ./", "Edit", repo, in("scripts/run.mjs")},
		{"entry with surrounding spaces", "MultiEdit", repo, in("client/app.js")},
		{"entry with a (new) note, nothing of that name existing", "Write", repo, in("hub/cmd/tt/tool_scope_test.go")},
		{"absolute entry", "Edit", repo, in("abs/file.go")},
		{"glob entry, nothing of that name existing", "Edit", repo, in("web/site.css")},
		{"the bracketed file itself", "Edit", repo, in("[id].go")},
		{"under the parenthesised directory", "Edit", repo, in("src (archive)/x.go")},
		{"relative path from a subdirectory", "Edit", in("hub/cmd/tt"), "tool_scope.go"},
		{"relative path climbing to an owned file", "NotebookEdit", in("hub/cmd/tt"), "../../../docs/n.ipynb"},
		{"outside the repository, absolute", "Write", repo, filepath.Join(outside, "notes.md")},
		{"outside the repository, an absolute entry names it", "Write", repo, filepath.Join(outside, "elsewhere", "x")},
		{"outside the repository, relative", "Edit", repo, "../sibling/file.go"},
		{"Bash", "Bash", repo, in("not/owned.go")},
		{"Read", "Read", repo, in("not/owned.go")},
		{"lower-case edit", "edit", repo, in("not/owned.go")},
	} {
		if got := call(c.tool, c.cwd, c.file); got != 0 {
			t.Fatalf("%s: %d would-report rows; want none: %v", c.name, got, toolLedgerRows(t, root, e.agent))
		}
	}
	// The same fixtures on disk give rows where an existing name is only itself.
	want := 0
	for _, c := range []struct{ name, file string }{
		{"entry [id].go exists, edit i.go", "i.go"},
		{"entry src (archive) exists, edit src/x.go", "src/x.go"},
		{"entry docs (new) exists as a directory, edit doc/a.md", "doc/a.md"},
		{"a glob does not cross a slash", "web/deep/site.css"},
		{"a prefix of an entry is not owned", "hub/cmd/tt"},
	} {
		want++
		if got := call("Edit", repo, in(c.file)); got != want {
			t.Fatalf("%s: %d would-report rows; want %d", c.name, got, want)
		}
	}
	// The entry "docs (new)" names an existing directory here, so only the
	// literal entry "docs/" owns docs/a.md.
	if got := call("Edit", repo, in("docs (new)/a.md")); got != want {
		t.Fatalf("under the existing directory docs (new): %d rows; want %d", got, want)
	}
}

// a5: the matching rules as a pure function of the entries, the path and
// what exists.
func TestToolScopeMatch(t *testing.T) {
	const root = "/repo"
	for _, c := range []struct {
		name     string
		entries  []string
		existing []string
		path     string
		owned    bool
	}{
		{"equal", []string{"a/b.go"}, nil, "a/b.go", true},
		{"under a directory", []string{"a"}, nil, "a/b/c.go", true},
		{"a longer name is not under it", []string{"a/b"}, nil, "a/bc", false},
		{"a parent is not owned", []string{"a/b/c.go"}, nil, "a/b", false},
		{"leading ./", []string{"./a/b.go"}, nil, "a/b.go", true},
		{"trailing /", []string{"a/"}, nil, "a/b.go", true},
		{"surrounding spaces", []string{"  a/b.go\t"}, nil, "a/b.go", true},
		{"unclean entry", []string{"a//x/../b.go"}, nil, "a/b.go", true},
		{"absolute entry inside the root", []string{"/repo/a/b.go"}, nil, "a/b.go", true},
		{"absolute entry outside the root is ignored", []string{"/elsewhere/a/b.go", "c"}, nil, "a/b.go", false},
		{"the root itself", []string{"."}, nil, "any/thing.go", true},
		{"the root as ./", []string{"./"}, nil, "any/thing.go", true},
		{"the root as an absolute entry", []string{"/repo"}, nil, "any/thing.go", true},
		{"second entry", []string{"x", "a/b.go"}, nil, "a/b.go", true},

		{"(new) note, nothing existing", []string{"a/b.go (new)"}, nil, "a/b.go", true},
		{"(new) note on a directory", []string{"a/ (new)"}, nil, "a/b.go", true},
		{"(new) note, the noted name exists", []string{"docs (new)"}, []string{"docs (new)"}, "docs/a.md", false},
		{"(new) note, the noted name exists, literal still owns", []string{"docs (new)"}, []string{"docs (new)"}, "docs (new)/a.md", true},
		{"(new) literal wins without a stat", []string{"docs (new)"}, nil, "docs (new)/a.md", true},
		{"only (new) is a note: (archive), existing", []string{"src (archive)"}, []string{"src (archive)"}, "src/x.go", false},
		{"only (new) is a note: (archive), nothing existing", []string{"src (archive)"}, nil, "src/x.go", false},
		{"(archive) directory owns what is under it", []string{"src (archive)"}, []string{"src (archive)"}, "src (archive)/x.go", true},
		{"(archive) directory, nothing existing", []string{"src (archive)"}, nil, "src (archive)/x.go", true},
		{"a note needs its space", []string{"a.go(new)"}, nil, "a.go", false},
		{"a note is at the end only", []string{"a (new) b"}, nil, "a", false},
		{"(NEW) is no note", []string{"a.go (NEW)"}, nil, "a.go", false},
		{"one note is dropped, not two", []string{"a.go (new) (new)"}, nil, "a.go", false},
		{"one note dropped leaves the inner name", []string{"a.go (new) (new)"}, nil, "a.go (new)", true},
		{"a note on a glob is not a glob", []string{"a/*.go (new)"}, nil, "a/b.go", false},

		{"glob", []string{"a/*.go"}, nil, "a/b.go", true},
		{"glob does not cross a slash", []string{"a/*.go"}, nil, "a/b/c.go", false},
		{"glob is over the whole path", []string{"*.go"}, nil, "a/b.go", false},
		{"glob at the top", []string{"*.go"}, nil, "b.go", true},
		{"question mark", []string{"a/?.go"}, nil, "a/b.go", true},
		{"class", []string{"a/[bc].go"}, nil, "a/c.go", true},
		{"bad pattern", []string{"a["}, nil, "ab", false},
		{"bad pattern, literal still owns", []string{"a["}, nil, "a[", true},
		{"bracketed file exists: only itself", []string{"[id].go"}, []string{"[id].go"}, "i.go", false},
		{"bracketed file exists: itself", []string{"[id].go"}, []string{"[id].go"}, "[id].go", true},
		{"bracketed file, nothing existing: itself", []string{"[id].go"}, nil, "[id].go", true},
		{"bracketed entry, nothing existing: a pattern", []string{"[id].go"}, nil, "i.go", true},
		{"starred name exists: only itself", []string{"a/*.go"}, []string{"a/*.go"}, "a/b.go", false},
		{"a glob does not own a directory's contents", []string{"a/*"}, nil, "a/b/c.go", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stats := 0
			exists := func(form string) bool {
				stats++
				for _, name := range c.existing {
					if name == form {
						return true
					}
				}
				return false
			}
			owned, usable := toolScopeOwned(root, c.path, c.entries, exists)
			if owned != c.owned || !usable {
				t.Fatalf("owned = %v, usable = %v; want %v, true", owned, usable, c.owned)
			}
			if stats > 2*len(c.entries) {
				t.Fatalf("%d stat calls for %d entries; want at most two each", stats, len(c.entries))
			}
		})
	}
	t.Run("plain entries make no stat call", func(t *testing.T) {
		entries := []string{"a/b.go", "docs/", "./c", "/repo/d", "src (archive)"}
		if owned, usable := toolScopeOwned(root, "x/y.go", entries, func(string) bool { t.Fatal("stat call"); return false }); owned || !usable {
			t.Fatalf("owned = %v, usable = %v", owned, usable)
		}
	})
	t.Run("nothing to judge against", func(t *testing.T) {
		for _, entries := range [][]string{nil, {""}, {"   ", "\t"}, {"../up", "/elsewhere/x", ".."}} {
			if owned, usable := toolScopeOwned(root, "a.go", entries, func(string) bool { return false }); owned || usable {
				t.Fatalf("%q: owned = %v, usable = %v; want false, false", entries, owned, usable)
			}
		}
	})
	t.Run("path field", func(t *testing.T) {
		big := strings.Repeat(`a\"b\\`, 1<<10)
		for raw, want := range map[string]string{
			`{"file_path":"/r/a.go"}`:                                        "/r/a.go",
			` { "content" : "` + big + `" , "file_path" : "/r/a.go" } `:      "/r/a.go",
			`{"file_path":"/r/first","file_path":"/r/last"}`:                 "/r/last",
			`{"edits":[{"file_path":"/r/nested"},["]"]],"file_path":"/r/a"}`: "/r/a",
			`{"n":1,"t":true,"z":null,"o":{"a":"}"},"file_path":"/r/é"}`:     "/r/é",
			`{"content":"\"file_path\":\"/r/quoted\""}`:                      "",
			`{"edits":[{"file_path":"/r/nested"}]}`:                          "",
			`{"file_path":7}`:                                                "",
			`{"file_path":null}`:                                             "",
			`{"file_path":["/r/a.go"]}`:                                      "",
			`{"file_path":""}`:                                               "",
			`["file_path","/r/a.go"]`:                                        "",
			`"file_path"`:                                                    "",
			`{"file_path":"/r/torn`:                                          "",
			`{"file_path"`:                                                   "",
			`{`:                                                              "",
			``:                                                               "",
		} {
			got, ok := toolScopeField([]byte(raw), "file_path")
			if got != want || ok != (want != "") && raw != `{"file_path":""}` {
				t.Errorf("%.60s: %q, %v; want %q", raw, got, ok, want)
			}
			// It agrees with the full decode wherever that succeeds.
			var full struct {
				FilePath *string `json:"file_path"`
			}
			if json.Unmarshal([]byte(raw), &full) == nil && full.FilePath != nil && *full.FilePath != got {
				t.Errorf("%.60s: %q; the full decode gives %q", raw, got, *full.FilePath)
			}
		}
	})
}

// scopeNormalRows drops what differs between two runs of the same calls.
func scopeNormalRows(rows []map[string]any) string {
	for _, row := range rows {
		delete(row, "time")
		delete(row, "durationMs")
		delete(row, "argsDigest") // the input names a fresh temp directory each run
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

// a6: every fail-open case leaves exactly the rows the same calls leave
// with the check switched off, and the hook stays silent.
func TestToolScopeFailsOpen(t *testing.T) {
	type sandbox struct {
		e          env
		root, repo string
		pre        map[string]any
	}
	valid := func(t *testing.T, s *sandbox) {
		toolScopeRemember(s.e, []api.Message{scopeAssign(s.e, 5, "owned.go")})
	}
	cacheFile := func(s *sandbox) string { return filepath.Join(s.root, s.e.agent, toolScopeCacheName) }
	rewrite := func(t *testing.T, s *sandbox, data []byte) {
		t.Helper()
		valid(t, s)
		if err := os.WriteFile(cacheFile(s), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// run makes the calls in a fresh sandbox and returns its normalised rows.
	run := func(t *testing.T, check bool, prepare func(*testing.T, *sandbox)) string {
		t.Helper()
		var s sandbox
		s.e, s.root = toolLedgerSandbox(t)
		s.repo = filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(filepath.Join(s.repo, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		s.pre = scopePre("Edit", "u1", s.repo, filepath.Join(s.repo, "outside.go"))
		if !check {
			setToolLedger(t, &toolScopeCheck, nil)
		}
		if prepare != nil {
			prepare(t, &s)
		}
		for _, event := range []string{"PreToolUse", "PostToolUse"} {
			if r := runToolHook(t, s.e, "tool", toolPayload(event, s.pre), nil); r.err != nil || r.out != "" {
				t.Fatalf("%s = %v, output %q; want nil and no output", event, r.err, r.out)
			}
		}
		rows := toolLedgerRows(t, s.root, s.e.agent)
		if last := rows[len(rows)-1]; last["outcome"] != "ok" || last["durationSource"] != "measured" || last["toolUseId"] != "u1" {
			t.Fatalf("the call's ordinary row is missing or unpaired: %v", rows)
		}
		return scopeNormalRows(rows)
	}
	if rows := run(t, true, valid); strings.Count(rows, `"would_report"`) != 1 {
		t.Fatalf("control: a valid cache gave %s; the cases below would prove nothing", rows)
	}
	for _, c := range []struct {
		name    string
		prepare func(*testing.T, *sandbox)
	}{
		{"missing cache", nil},
		{"invalid cache", func(t *testing.T, s *sandbox) {
			rewrite(t, s, []byte(`{"v":1,"run":"run_ledger","assignSeq":5,"owns":["owned.go"`))
		}},
		{"cache of another version", func(t *testing.T, s *sandbox) {
			rewrite(t, s, []byte(`{"v":2,"run":"run_ledger","assignSeq":5,"owns":["owned.go"]}`))
		}},
		{"cache over the cap", func(t *testing.T, s *sandbox) {
			pad := strings.Repeat(" ", toolScopeCacheBytes)
			rewrite(t, s, []byte(`{"v":1,"run":"run_ledger","assignSeq":5,"owns":["owned.go"]}`+pad))
		}},
		{"unreadable cache", func(t *testing.T, s *sandbox) {
			if os.Getuid() == 0 {
				t.Skip("root reads a mode 000 file")
			}
			valid(t, s)
			if err := os.Chmod(cacheFile(s), 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"cache is a directory", func(t *testing.T, s *sandbox) {
			if err := os.MkdirAll(cacheFile(s), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"overflow cache", func(t *testing.T, s *sandbox) {
			toolScopeRemember(s.e, []api.Message{scopeAssign(s.e, 5, make([]string, toolScopeMaxEntries+1)...)})
			if cache, _ := scopeReadCache(t, s.root, s.e.agent); !cache.Overflow {
				t.Fatal("fixture is not an overflow cache")
			}
		}},
		{"cache with no entries", func(t *testing.T, s *sandbox) { rewrite(t, s, []byte(`{"v":1,"run":"run_ledger","assignSeq":5}`)) }},
		{"cache with no usable entry", func(t *testing.T, s *sandbox) {
			toolScopeRemember(s.e, []api.Message{scopeAssign(s.e, 5, "  ", "../up", "/elsewhere")})
		}},
		{"cache of another run", func(t *testing.T, s *sandbox) {
			other := s.e
			other.runID = "run_previous"
			toolScopeRemember(other, []api.Message{scopeAssign(s.e, 5, "owned.go")})
		}},
		{"missing cwd", func(t *testing.T, s *sandbox) { valid(t, s); s.pre["cwd"] = "" }},
		{"relative cwd", func(t *testing.T, s *sandbox) { valid(t, s); s.pre["cwd"] = "repo" }},
		{"cwd is null", func(t *testing.T, s *sandbox) { valid(t, s); s.pre["cwd"] = nil }},
		{"cwd is a number", func(t *testing.T, s *sandbox) { valid(t, s); s.pre["cwd"] = 7 }},
		{"cwd is an object", func(t *testing.T, s *sandbox) { valid(t, s); s.pre["cwd"] = map[string]any{"path": s.repo} }},
		{"cwd is an array", func(t *testing.T, s *sandbox) { valid(t, s); s.pre["cwd"] = []string{s.repo} }},
		{"cwd is a bool", func(t *testing.T, s *sandbox) { valid(t, s); s.pre["cwd"] = true }},
		{"no .git above cwd", func(t *testing.T, s *sandbox) {
			valid(t, s)
			if err := os.Remove(filepath.Join(s.repo, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
		{"no path field", func(t *testing.T, s *sandbox) {
			valid(t, s)
			s.pre["tool_input"] = map[string]any{"old_string": "a", "notebook_path": filepath.Join(s.repo, "outside.ipynb")}
		}},
		{"empty path", func(t *testing.T, s *sandbox) {
			valid(t, s)
			s.pre["tool_input"] = map[string]any{"file_path": ""}
		}},
		{"path of another type", func(t *testing.T, s *sandbox) {
			valid(t, s)
			s.pre["tool_input"] = map[string]any{"file_path": []string{filepath.Join(s.repo, "outside.go")}}
		}},
		{"no tool input", func(t *testing.T, s *sandbox) { valid(t, s); delete(s.pre, "tool_input") }},
		{"the check panics", func(t *testing.T, s *sandbox) {
			valid(t, s)
			if toolScopeCheck == nil {
				return // the run with the check switched off
			}
			setToolLedger(t, &toolScopeCheck, func(toolLedger, toolLedgerRow, string, json.RawMessage, string) { panic("scope check on fire") })
		}},
		{"a write panics inside the check", func(t *testing.T, s *sandbox) {
			valid(t, s)
			setToolLedger(t, &toolLedgerWrite, func(f *os.File, b []byte) error {
				if strings.Contains(string(b), toolScopeOutcome) {
					panic("disk on fire")
				}
				_, err := f.Write(b)
				return err
			})
		}},
		{"deadline already passed", func(t *testing.T, s *sandbox) {
			valid(t, s)
			setToolLedger(t, &toolLedgerDeadline, 40*time.Millisecond)
			finished := make(chan struct{})
			real := toolScopeCheck
			if real == nil {
				return // the run with the check switched off
			}
			var once sync.Once
			setToolLedger(t, &toolScopeCheck, func(l toolLedger, row toolLedgerRow, tool string, input json.RawMessage, cwd string) {
				if tool != "Edit" {
					return
				}
				time.Sleep(time.Until(l.started.Add(l.deadline + 20*time.Millisecond)))
				real(l, row, tool, input, cwd)
				once.Do(func() { close(finished) })
			})
			t.Cleanup(func() {
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("the late check never finished")
				}
			})
		}},
		{"the path scan cannot finish", func(t *testing.T, s *sandbox) {
			valid(t, s)
			setToolLedger(t, &toolRedactScanBudget, 0)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var off, on string
			t.Run("check off", func(t *testing.T) { off = run(t, false, c.prepare) })
			t.Run("check on", func(t *testing.T) { on = run(t, true, c.prepare) })
			if off == "" || on == "" {
				t.Skip("a run was skipped")
			}
			if off != on || strings.Contains(on, toolScopeOutcome) {
				t.Fatalf("rows with the check differ:\noff %s\non  %s", off, on)
			}
		})
	}
	t.Run("called directly with nothing set", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		repo := scopeRepo(t)
		toolScopeRemember(e, []api.Message{scopeAssign(e, 5, "owned.go")})
		// l.now and l.write are nil: the append panics inside the check.
		l := toolLedger{dir: filepath.Join(root, e.agent), agent: e.agent, run: e.runID, started: time.Now(), deadline: time.Minute, redactBudget: time.Minute}
		l.wouldReport(toolLedgerRow{}, "Edit", json.RawMessage(mustJSON(t, map[string]any{"file_path": filepath.Join(repo, "outside.go")})), repo)
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != 0 {
			t.Fatalf("rows = %v", rows)
		}
	})
}

// a6: matching and non-matching calls alike return nil and print nothing,
// on all three events.
func TestToolScopePrintsNothing(t *testing.T) {
	e, root := toolLedgerSandbox(t)
	repo := scopeRepo(t)
	toolScopeRemember(e, []api.Message{scopeAssign(e, 5, "owned.go")})
	n := 0
	for _, tool := range []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "Bash"} {
		for _, file := range []string{"owned.go", "outside.go", "../elsewhere.go"} {
			n++
			call := scopePre(tool, fmt.Sprintf("u%d", n), repo, filepath.Join(repo, file))
			for _, event := range []string{"PreToolUse", "PostToolUse", "PreToolUse", "PostToolUseFailure"} {
				if r := runToolHook(t, e, "tool", toolPayload(event, call), nil); r.err != nil || r.out != "" {
					t.Fatalf("%s %s %s = %v, output %q; want nil and no output", event, tool, file, r.err, r.out)
				}
			}
		}
	}
	rows := toolLedgerRows(t, root, e.agent)
	if got := len(wouldReportRows(rows)); got != 2*4 {
		t.Fatalf("would-report rows = %d; want 8 (four edit tools, two pres each, outside.go only)", got)
	}
	if got := len(rows) - len(wouldReportRows(rows)); got != 2*n {
		t.Fatalf("ordinary rows = %d; want %d", got, 2*n)
	}
}

// a6: the scope row is never written after the hook's deadline, whether the
// time goes on waiting for the ledger lock or between the check and the write.
func TestToolScopeAppendDeadline(t *testing.T) {
	// finish wraps the check so the test knows when the goroutine the hook
	// left behind has returned.
	finish := func(t *testing.T) chan struct{} {
		finished := make(chan struct{}, 8)
		real := toolScopeCheck
		setToolLedger(t, &toolScopeCheck, func(l toolLedger, row toolLedgerRow, tool string, input json.RawMessage, cwd string) {
			real(l, row, tool, input, cwd)
			finished <- struct{}{}
		})
		return finished
	}
	wait := func(t *testing.T, finished chan struct{}) {
		t.Helper()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatal("the check never returned")
		}
	}
	ordinary := func(t *testing.T, e env, root string, pre map[string]any) {
		t.Helper()
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != 0 {
			t.Fatalf("rows = %v; want none", rows)
		}
		time.Sleep(200 * time.Millisecond)
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != 0 {
			t.Fatalf("rows 200 ms later = %v; want none", rows)
		}
		setToolLedger(t, &toolScopeSince, time.Since)
		setToolLedger(t, &toolLedgerDeadline, 150*time.Millisecond)
		setToolLedger(t, &toolLedgerLockWait, 50*time.Millisecond)
		toolHook(t, e, "PostToolUse", pre)
		if rows := toolLedgerRows(t, root, e.agent); len(rows) != 1 || rows[0]["outcome"] != "ok" || rows[0]["durationSource"] != "measured" {
			t.Fatalf("rows after the post = %v; want the call's ordinary row", rows)
		}
	}
	for _, file := range []string{"outside.go", "owned.go"} {
		t.Run("ledger lock held past the deadline, "+file, func(t *testing.T) {
			e, root := toolLedgerSandbox(t)
			repo := scopeRepo(t)
			toolScopeRemember(e, []api.Message{scopeAssign(e, 5, "owned.go")})
			// The ledger's own lock wait is far longer than the deadline, so
			// an append that was not capped would write late once it gave up.
			setToolLedger(t, &toolLedgerDeadline, 60*time.Millisecond)
			setToolLedger(t, &toolLedgerLockWait, 400*time.Millisecond)
			finished := finish(t)
			release := scopeHoldLock(t, root, e.agent, "ledger.lock")
			pre := scopePre("Edit", "u1", repo, filepath.Join(repo, file))
			r := runToolHook(t, e, "tool", toolPayload("PreToolUse", pre), nil)
			if r.err != nil || r.out != "" {
				t.Fatalf("hook = %v, output %q; want nil and no output", r.err, r.out)
			}
			wait(t, finished)
			release()
			ordinary(t, e, root, pre)
		})
	}
	t.Run("deadline passes between the check and the write", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		repo := scopeRepo(t)
		toolScopeRemember(e, []api.Message{scopeAssign(e, 5, "owned.go")})
		finished := finish(t)
		var reads atomic.Int32
		setToolLedger(t, &toolScopeSince, func(start time.Time) time.Duration {
			if reads.Add(1) == 1 {
				return time.Millisecond // on entry to the append: well inside the deadline
			}
			return toolLedgerDeadline // immediately before the write: the deadline has passed
		})
		pre := scopePre("Edit", "u1", repo, filepath.Join(repo, "outside.go"))
		toolHook(t, e, "PreToolUse", pre)
		wait(t, finished)
		if got := reads.Load(); got != 2 {
			t.Fatalf("the deadline was read %d times; want on entry and before the write", got)
		}
		ordinary(t, e, root, pre)
	})
	t.Run("control: inside the deadline the row is written", func(t *testing.T) {
		e, root := toolLedgerSandbox(t)
		repo := scopeRepo(t)
		toolScopeRemember(e, []api.Message{scopeAssign(e, 5, "owned.go")})
		finished := finish(t)
		var reads atomic.Int32
		setToolLedger(t, &toolScopeSince, func(time.Time) time.Duration { reads.Add(1); return time.Millisecond })
		toolHook(t, e, "PreToolUse", scopePre("Edit", "u1", repo, filepath.Join(repo, "outside.go")))
		wait(t, finished)
		if rows := wouldReportRows(toolLedgerRows(t, root, e.agent)); len(rows) != 1 || reads.Load() != 2 {
			t.Fatalf("rows = %v after %d deadline reads", rows, reads.Load())
		}
	})
}

// a7: the added cost of the check per PreToolUse, measured against the same
// hook with the check replaced by nothing, by the method of TestToolDenyCost.
// Every run with the check returns before the hook's deadline with its row
// already written.
//
// The 5 ms limit is for the binary that ships. Under the race detector the
// hook's own decode of a 1 MiB input takes over 100 ms before the check
// starts, which is past the path scan's budget, so the check would rightly
// write no row. There the limit, the hook's deadline and the scan budget
// are all five times looser, and the same rows are required.
func TestToolScopeCost(t *testing.T) {
	const runs = 100
	limit := 5 * time.Millisecond
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "-race" && setting.Value == "true" {
				limit = 25 * time.Millisecond
				setToolLedger(t, &toolLedgerDeadline, 5*toolLedgerDeadline)
				setToolLedger(t, &toolRedactScanBudget, 5*toolRedactScanBudget)
				t.Logf("race detector on: the added-cost limit is %v, the hook deadline %v and the scan budget %v", limit, toolLedgerDeadline, toolRedactScanBudget)
			}
		}
	}
	e, root := toolLedgerSandbox(t)
	repo := scopeRepo(t)
	plain, globs := make([]string, toolScopeMaxEntries), make([]string, toolScopeMaxEntries)
	for i := range plain {
		plain[i] = fmt.Sprintf("pkg/dir%03d/file.go", i)
		globs[i] = fmt.Sprintf("pkg/dir%03d/*.go", i)
	}
	big := scopePre("Write", "u1", repo, filepath.Join(repo, "outside/big.txt"))
	big["tool_input"] = map[string]any{"file_path": filepath.Join(repo, "outside/big.txt"), "content": strings.Repeat("a line of \"quoted\" content\n", (1<<20)/27)}
	real := toolScopeCheck
	cases := []struct {
		name string
		owns []string
		pre  map[string]any
		rows int
	}{
		{"Bash with a cache", plain, bashPre("u1", "go test ./cmd/tt -run TestToolScope -count=1"), 0},
		{"Edit with no cache", nil, scopePre("Edit", "u1", repo, filepath.Join(repo, "outside/a.go")), 0},
		{"Edit owned, 256 entries", plain, scopePre("Edit", "u1", repo, filepath.Join(repo, "pkg/dir255/file.go")), 0},
		{"Edit outside, 256 entries", plain, scopePre("Edit", "u1", repo, filepath.Join(repo, "outside/a.go")), 1},
		{"Write outside, 1 MiB of content", plain, big, 1},
		{"Edit outside, 256 glob entries", globs, scopePre("Edit", "u1", repo, filepath.Join(repo, "outside/a.go")), 1},
	}
	for n, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.pre["cwd"] = repo
			payload := toolPayload("PreToolUse", c.pre)
			var with, without []time.Duration
			late := 0
			var worst time.Duration
			for i := 0; i < 2*runs; i++ {
				// Alternate, and swap which goes first each pair.
				check := (i+i/2)%2 == 0
				e.agent = fmt.Sprintf("agt_scope_cost_%d_%d", n, i)
				if c.owns != nil {
					toolScopeRemember(e, []api.Message{scopeAssign(e, 5, c.owns...)})
				}
				toolScopeCheck = nil
				if check {
					toolScopeCheck = real
				}
				r := runToolHook(t, e, "tool", payload, nil)
				toolScopeCheck = real
				if r.err != nil || r.out != "" {
					t.Fatalf("hook = %v, output %q", r.err, r.out)
				}
				rows := len(wouldReportRows(toolLedgerRows(t, root, e.agent)))
				switch {
				case !check:
					if rows != 0 {
						t.Fatalf("baseline run wrote %d would-report rows", rows)
					}
					without = append(without, r.elapsed)
				case r.elapsed >= toolLedgerDeadline:
					late++
					t.Logf("run %d reached the %v deadline in %v with %d of %d rows", i, toolLedgerDeadline, r.elapsed, rows, c.rows)
				default:
					if rows != c.rows {
						t.Fatalf("run %d returned in %v with %d would-report rows; want %d", i, r.elapsed, rows, c.rows)
					}
					with = append(with, r.elapsed)
					worst = max(worst, r.elapsed)
				}
			}
			if late > 2 {
				t.Fatalf("%d of %d runs reached the hook deadline; at most 2 are put down to a stalled host", late, runs)
			}
			base, checked := toolDenyMedian(without), toolDenyMedian(with)
			t.Logf("%d-byte input: baseline median %v, with the check %v, added %v, worst run with the check %v", len(payload), base, checked, checked-base, worst)
			if checked-base >= limit {
				t.Fatalf("the check adds %v to the median hook; want under %v", checked-base, limit)
			}
		})
	}
}

// a8: the hook opens no hub connection, and the new file can neither start
// a process nor open a socket, nor name the hub's config file.
func TestToolScopeNoHubNoProcess(t *testing.T) {
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
	e, root := toolLedgerSandbox(t)
	e.hub = "http://" + ln.Addr().String()
	repo := scopeRepo(t)
	toolScopeRemember(e, []api.Message{scopeAssign(e, 5, "owned.go", "web/*.css")})
	for i, file := range []string{"outside.go", "owned.go", "web/site.css", "../elsewhere.go"} {
		call := scopePre("Edit", fmt.Sprintf("u%d", i), repo, filepath.Join(repo, file))
		for _, event := range []string{"PreToolUse", "PostToolUse"} {
			if r := runToolHook(t, e, "tool", toolPayload(event, call), nil); r.err != nil || r.out != "" || r.elapsed >= toolLedgerBound {
				t.Fatalf("%s %s = %v, output %q, in %v", event, file, r.err, r.out, r.elapsed)
			}
		}
	}
	if rows := toolLedgerRows(t, root, e.agent); len(rows) != 5 || len(wouldReportRows(rows)) != 1 {
		t.Fatalf("rows = %v; want four calls and one report", rows)
	}
	if n := connections.Load(); n != 0 {
		t.Fatalf("the hook opened %d hub connection(s)", n)
	}
	src, err := os.ReadFile("tool_scope.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "tool_scope.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range file.Imports {
		switch name := strings.Trim(imp.Path.Value, `"`); {
		case name == "os/exec", name == "net", strings.HasPrefix(name, "net/"), name == "syscall/js", name == "plugin", strings.Contains(name, "/internal/spawn"):
			t.Errorf("tool_scope.go imports %s", name)
		}
	}
	for _, banned := range []string{"hub" + ".json", "exec.Command", "StartProcess", "ForkExec", "syscall.Exec", "e.client(", "api.NewClient"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("tool_scope.go names %s", banned)
		}
	}
}

// a9: the command docs/claude-wake.md gives counts would-report rows by rule
// and agent, across agents and a rotated file, past a torn last line.
func TestToolScopeDocCommand(t *testing.T) {
	const jq = "/usr/bin/jq"
	if _, err := os.Stat(jq); err != nil {
		t.Skipf("%s is not installed", jq)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "claude-wake.md"))
	if err != nil {
		t.Fatal(err)
	}
	command := ""
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.Contains(line, `select(.outcome == "would_report")`) {
			if command != "" {
				t.Fatal("docs/claude-wake.md gives the would-report summary command more than once")
			}
			command = line
		}
	}
	if !strings.HasPrefix(command, "awk 1 ~/.local/state/tailterm/tool-ledger/") || !strings.Contains(command, jq) || strings.Contains(command, "would_deny") {
		t.Fatalf("docs/claude-wake.md does not give the would-report summary command: %q", command)
	}
	home := t.TempDir()
	write := func(agent, name string, lines ...string) {
		dir := filepath.Join(home, ".local", "state", "tailterm", "tool-ledger", agent)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	row := func(agent, outcome, rule string) string {
		r := toolLedgerRow{V: 1, Agent: agent, Tool: "Edit", Outcome: outcome}
		switch outcome {
		case "would_report":
			r.ReportRule, r.ReportPath, r.AssignSeq = rule, "docs/a.md", 7
		case "would_deny":
			r.Tool, r.DenyRule = "Bash", rule
		}
		b, _ := json.Marshal(r)
		return string(b)
	}
	write("agt_a", "ledger.jsonl.1", row("agt_a", "would_report", "outside-owned-paths"), row("agt_a", "ok", ""), "")
	write("agt_a", "ledger.jsonl", row("agt_a", "would_report", "outside-owned-paths"), row("agt_a", "would_deny", "host-pkill"), row("agt_a", "would_report", "a-later-rule"), row("agt_a", "unknown", ""), `{"v":1,"agent":"agt_a","outcome":"would_rep`)
	write("agt_b", "ledger.jsonl", row("agt_b", "ok", ""), row("agt_b", "would_report", "outside-owned-paths"), "")
	write("agt_b", toolScopeCacheName, `{"v":1,"run":"r","assignSeq":7,"item":"","itemRevision":0,"time":"t","owns":["a"]}`)
	write("agt_c", "ledger.jsonl", row("agt_c", "ok", ""), row("agt_c", "would_deny", "host-pkill"), "")
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", command, err, out)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	want := []string{"1 a-later-rule agt_a", "2 outside-owned-paths agt_a", "1 outside-owned-paths agt_b"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("summary = %q; want %q", got, want)
	}
}
