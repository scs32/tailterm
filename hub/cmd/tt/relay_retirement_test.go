package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayRetirementOnceStopsPolling(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", dir)
	t.Setenv("TT_TMUX_SOCKET", "retirement-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	for _, k := range []string{"TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN", "TAILTERM_TOKEN"} {
		t.Setenv(k, "")
	}
	b := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/nonexistent"}
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/team-queues":
			w.Write([]byte(`{"entries":[]}`))
		case strings.Contains(r.URL.Path, "/agents/"):
			polls.Add(1)
			json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, TaskID: b.Task, RunID: b.Run, Status: api.AgentClosed, CleanupDone: true})
		case strings.HasSuffix(r.URL.Path, "/pause"):
			json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	t.Setenv("TAILTERM_HUB", server.URL)
	path := filepath.Join(dir, bindingKey(b)+".binding.json")
	if err := writePrivateJSON(path, b); err != nil {
		t.Fatal(err)
	}
	if err := cmdRelay([]string{"--once"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("obsolete binding remains: %v", err)
	}
	count := polls.Load()
	if count != 1 {
		t.Fatalf("first pass agent polls=%d want 1", count)
	}
	if err := cmdRelay([]string{"--once"}); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != count {
		t.Fatalf("archived binding still polled: %d -> %d", count, polls.Load())
	}
}

func retirementFixture(t *testing.T) (string, string, runtimeBinding, []byte, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "relay")
	t.Setenv("TAILTERM_RELAY_STATE", dir)
	b := runtimeBinding{Hub: "http://localhost:18765", Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/nonexistent"}
	path := filepath.Join(dir, bindingKey(b)+".binding.json")
	if err := writeRelayBinding(b); err != nil {
		t.Fatal(err)
	}
	pp := filepath.Join(dir, bindingKey(b)+".progress.json")
	if err := writePrivateJSON(pp, relayProgress{Run: b.Run, Thread: b.Thread, Through: 73}); err != nil {
		t.Fatal(err)
	}
	bd, _ := os.ReadFile(path)
	pd, _ := os.ReadFile(pp)
	return dir, path, b, bd, pd
}

func TestRelayRetirementStatusAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, status, run string
		cleanup, retire   bool
		missing           string
	}{
		{name: "closed", status: api.AgentClosed, cleanup: true, retire: true},
		{name: "exited", status: api.AgentExited, cleanup: true, retire: true},
		{name: "closed pending", status: api.AgentClosed},
		{name: "exited pending", status: api.AgentExited},
		{name: "running", status: api.AgentRunning},
		{name: "done", status: api.AgentDone},
		{name: "needs input", status: api.AgentNeedsInput},
		{name: "retired reuse", status: api.AgentRetired},
		{name: "live successor", status: api.AgentRunning, run: "run_0000000000000002", retire: true},
		{name: "retired successor", status: api.AgentRetired, run: "run_0000000000000002", retire: true},
		{name: "unknown", status: "unknown", run: "run_0000000000000002"},
		{name: "missing status", cleanup: true},
		{name: "missing agent", status: api.AgentClosed, cleanup: true, missing: "agent"},
		{name: "missing task", status: api.AgentClosed, cleanup: true, missing: "task"},
		{name: "missing run", status: api.AgentClosed, cleanup: true, missing: "run"},
		{name: "invalid run", status: api.AgentClosed, cleanup: true, run: "garbage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, path, b, bd, pd := retirementFixture(t)
			a := api.Agent{ID: b.Agent, TaskID: b.Task, RunID: b.Run, Status: tc.status, CleanupDone: tc.cleanup}
			if tc.run != "" {
				a.RunID = tc.run
			}
			switch tc.missing {
			case "agent":
				a.ID = ""
			case "task":
				a.TaskID = ""
			case "run":
				a.RunID = ""
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(a) }))
			defer server.Close()
			c, err := api.NewClient(server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			logs, err := captureRelayOutput(t, true, func() error {
				retired, e := retireRelayBinding(context.Background(), dir, path, b, c)
				if retired != tc.retire {
					t.Errorf("retired=%v want %v", retired, tc.retire)
				}
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.retire {
				archives, _ := filepath.Glob(filepath.Join(relayArchiveDir(dir), "*", "binding.json"))
				if len(archives) != 1 {
					t.Fatalf("archives=%v", archives)
				}
				got, _ := os.ReadFile(archives[0])
				if !bytes.Equal(got, bd) {
					t.Fatal("binding bytes changed")
				}
				got, _ = os.ReadFile(filepath.Join(filepath.Dir(archives[0]), "progress.json"))
				if !bytes.Equal(got, pd) {
					t.Fatal("progress bytes changed")
				}
				if !strings.Contains(logs, "[tt relay] retired "+b.Agent) {
					t.Fatalf("missing retirement log: %s", logs)
				}
				if _, err := os.Stat(filepath.Join(dir, bindingKey(b)+".progress.json")); !os.IsNotExist(err) {
					t.Fatal("active progress remains")
				}
			} else {
				got, _ := os.ReadFile(path)
				if !bytes.Equal(got, bd) {
					t.Fatal("retained binding changed")
				}
			}
		})
	}
}

func TestRelayRetirementHubErrorsRetain(t *testing.T) {
	for _, code := range []int{404, 429, 500, 503, 0} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			dir, path, b, bd, _ := retirementFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", code) }))
			if code == 0 {
				server.Close()
			} else {
				defer server.Close()
			}
			c, _ := api.NewClient(server.URL, time.Second)
			retired, err := retireRelayBinding(context.Background(), dir, path, b, c)
			if err == nil || retired {
				t.Fatalf("retired=%v err=%v", retired, err)
			}
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, bd) || relayArchiveCount(dir) != 0 {
				t.Fatal("hub error changed binding")
			}
		})
	}
}

func TestRelayRetirementRecoversPartialMoveBeforeSuccessor(t *testing.T) {
	dir, path, b, bd, pd := retirementFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, TaskID: b.Task, RunID: b.Run, Status: api.AgentClosed, CleanupDone: true})
	}))
	defer server.Close()
	c, _ := api.NewClient(server.URL, time.Second)
	original := relayArchiveRename
	relayArchiveRename = func(src, dst string) error {
		if strings.HasSuffix(src, ".binding.json") {
			return errors.New("injected move failure")
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { relayArchiveRename = original })
	retired, err := retireRelayBinding(context.Background(), dir, path, b, c)
	if !retired || err == nil {
		t.Fatalf("retired=%v err=%v", retired, err)
	}
	if err := saveRelayProgress(dir, filepath.Join(dir, bindingKey(b)+".progress.json"), b, relayProgress{Run: b.Run, Thread: b.Thread}); err == nil {
		t.Fatal("pending move should block progress writer")
	}
	relayArchiveRename = original
	successor := b
	successor.Run = "run_0000000000000002"
	successor.Thread = "00000000-0000-4000-8000-000000000002"
	if err := writeRelayBinding(successor); err != nil {
		t.Fatal(err)
	}
	if err := saveRelayProgress(dir, filepath.Join(dir, bindingKey(b)+".progress.json"), successor, relayProgress{Run: successor.Run, Thread: successor.Thread, Through: 99}); err != nil {
		t.Fatal(err)
	}
	if err := saveRelayProgress(dir, filepath.Join(dir, bindingKey(b)+".progress.json"), b, relayProgress{Run: b.Run, Thread: b.Thread}); err != nil {
		t.Fatal(err)
	}
	if err := recoverRelayRetirements(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	var current runtimeBinding
	json.Unmarshal(got, &current)
	if current != successor {
		t.Fatal("successor binding lost")
	}
	got, _ = os.ReadFile(filepath.Join(dir, bindingKey(b)+".progress.json"))
	var p relayProgress
	json.Unmarshal(got, &p)
	if p.Run != successor.Run || p.Through != 99 {
		t.Fatal("successor progress lost")
	}
	archives, _ := filepath.Glob(filepath.Join(relayArchiveDir(dir), "*", "binding.json"))
	if len(archives) != 1 {
		t.Fatal(archives)
	}
	got, _ = os.ReadFile(archives[0])
	if !bytes.Equal(got, bd) {
		t.Fatal("binding changed")
	}
	got, _ = os.ReadFile(filepath.Join(filepath.Dir(archives[0]), "progress.json"))
	if !bytes.Equal(got, pd) {
		t.Fatal("progress changed")
	}
}

func TestRelayRetirementReplacementDuringLookup(t *testing.T) {
	dir, path, b, _, _ := retirementFixture(t)
	successor := b
	successor.Run = "run_0000000000000002"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := writeRelayBinding(successor); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, TaskID: b.Task, RunID: successor.Run, Status: api.AgentRunning})
	}))
	defer server.Close()
	c, _ := api.NewClient(server.URL, time.Second)
	skip, err := retireRelayBinding(context.Background(), dir, path, b, c)
	if err != nil || !skip || relayArchiveCount(dir) != 0 {
		t.Fatalf("skip=%v err=%v", skip, err)
	}
	got, _ := os.ReadFile(path)
	var current runtimeBinding
	json.Unmarshal(got, &current)
	if current != successor {
		t.Fatal("replacement lost")
	}
}

func TestRelayRetirementArchiveCollision(t *testing.T) {
	dir, path, b, bd, pd := retirementFixture(t)
	archive := filepath.Join(relayArchiveDir(dir), bindingKey(b)+"-collision")
	os.MkdirAll(archive, 0700)
	os.WriteFile(filepath.Join(archive, "progress.json"), []byte("existing"), 0600)
	r := relayRetirement{Binding: b, BindingDigest: relayFileDigest(bd), ProgressDigest: relayFileDigest(pd)}
	if err := finishRelayRetirement(dir, archive, r); err == nil {
		t.Fatal("collision overwritten")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, bd) {
		t.Fatal("binding lost")
	}
	got, _ = os.ReadFile(filepath.Join(archive, "progress.json"))
	if string(got) != "existing" {
		t.Fatal("destination lost")
	}
}

func TestRelayRetirementStatusCensusAndMissingProgress(t *testing.T) {
	dir, path, b, _, _ := retirementFixture(t)
	domain, _ := canonicalLimiterDomain(b.Hub)
	before := hostRelayCensus(dir, "test", domain, time.Now())
	os.Remove(filepath.Join(dir, bindingKey(b)+".progress.json"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, TaskID: b.Task, RunID: b.Run, Status: api.AgentExited, CleanupDone: true})
	}))
	defer server.Close()
	c, _ := api.NewClient(server.URL, time.Second)
	if retired, err := retireRelayBinding(context.Background(), dir, path, b, c); err != nil || !retired {
		t.Fatalf("retired=%v err=%v", retired, err)
	}
	if err := saveRelayProgress(dir, filepath.Join(dir, bindingKey(b)+".progress.json"), b, relayProgress{Run: b.Run, Thread: b.Thread}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, bindingKey(b)+".progress.json")); !os.IsNotExist(err) {
		t.Fatal("retired progress recreated")
	}
	after := hostRelayCensus(dir, "test", domain, time.Now())
	if before.RelayBindings != 1 || after.RelayBindings != 0 || !after.Complete || before.SourceDigest == after.SourceDigest {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	live := b
	live.Agent = "agt_0000000000000002"
	if err := writeRelayBinding(live); err != nil {
		t.Fatal(err)
	}
	output, err := captureRelayOutput(t, false, func() error { return cmdRelay([]string{"--status"}) })
	if err != nil || !strings.Contains(output, live.Agent) || strings.Contains(output, b.Agent) || !strings.Contains(output, "archived bindings=1") {
		t.Fatalf("status=%q err=%v", output, err)
	}
	os.WriteFile(filepath.Join(dir, "broken.binding.json"), []byte("{"), 0600)
	if census := hostRelayCensus(dir, "test", domain, time.Now()); census.Complete {
		t.Fatal("unreadable active safeguard lost")
	}
}

func TestRelayRetirementBindingWriterUsesSameLock(t *testing.T) {
	dir, path, b, _, _ := retirementFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- withRelayBindingLock(dir, bindingKey(b), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	successor := b
	successor.Run = "run_0000000000000002"
	started := make(chan struct{})
	written := make(chan error, 1)
	go func() { close(started); written <- writeRelayBinding(successor) }()
	<-started
	select {
	case err := <-written:
		close(release)
		t.Fatalf("writer bypassed lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var current runtimeBinding
	json.Unmarshal(data, &current)
	if current != successor {
		t.Fatal("successor not saved")
	}
}

func TestRelayRetirementPreservesUnmatchedProgress(t *testing.T) {
	dir, path, b, _, _ := retirementFixture(t)
	pp := filepath.Join(dir, bindingKey(b)+".progress.json")
	data := []byte(`{"run":"run_0000000000000002","thread":"00000000-0000-4000-8000-000000000002","queuedThrough":91}`)
	os.WriteFile(pp, data, 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, TaskID: b.Task, RunID: "run_0000000000000002", Status: api.AgentDone})
	}))
	defer server.Close()
	c, _ := api.NewClient(server.URL, time.Second)
	if retired, err := retireRelayBinding(context.Background(), dir, path, b, c); err != nil || !retired {
		t.Fatalf("retired=%v err=%v", retired, err)
	}
	got, _ := os.ReadFile(pp)
	if !bytes.Equal(got, data) {
		t.Fatal("unmatched progress lost")
	}
}

// Reviewer experiment: count hub requests made by one --once pass for live
// (running, matching-run) bindings. Run at base and candidate and compare.
func TestRelayRetirementLiveBindingRequestCounts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", dir)
	t.Setenv("TT_TMUX_SOCKET", "review-counts-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	for _, k := range []string{"TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN", "TAILTERM_TOKEN"} {
		t.Setenv(k, "")
	}
	codex := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/nonexistent", Runtime: "codex"}
	claude := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000002", Run: "run_0000000000000002", Thread: "00000000-0000-4000-8000-000000000002", Runtime: "claude", Session: "review-claude"}
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		key = strings.ReplaceAll(key, codex.Agent, "<codex>")
		key = strings.ReplaceAll(key, claude.Agent, "<claude>")
		mu.Lock()
		counts[key]++
		mu.Unlock()
		switch {
		case r.URL.Path == "/v1/team-queues":
			w.Write([]byte(`{"entries":[]}`))
		case strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/agents/"+codex.Agent):
			json.NewEncoder(w).Encode(api.Agent{ID: codex.Agent, TaskID: codex.Task, RunID: codex.Run, Status: api.AgentRunning, Online: true, Runtime: "codex"})
		case strings.Contains(r.URL.Path, "/agents/"+claude.Agent):
			json.NewEncoder(w).Encode(api.Agent{ID: claude.Agent, TaskID: claude.Task, RunID: claude.Run, Status: api.AgentRunning, Online: true, Runtime: "claude", Session: claude.Session})
		case strings.HasSuffix(r.URL.Path, "/pause"):
			json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	codex.Hub, claude.Hub = server.URL, server.URL
	t.Setenv("TAILTERM_HUB", server.URL)
	for _, b := range []runtimeBinding{codex, claude} {
		if err := writePrivateJSON(filepath.Join(dir, bindingKey(b)+".binding.json"), b); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = captureRelayOutput(t, true, func() error { return cmdRelay([]string{"--once"}) })
	var keys []string
	total := 0
	for k, n := range counts {
		keys = append(keys, k)
		total += n
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-70s %d", k, counts[k])
	}
	t.Logf("TOTAL requests in one --once pass: %d", total)
	if total != 10 {
		t.Errorf("first unexamined pass=%d, want base8 plus one initial probe per binding", total)
	}
	// Reset delivery/activity cadence to the same first-pass conditions, keeping
	// only the saved probe deadline: the repeated --once must add no agent reads.
	for _, b := range []runtimeBinding{codex, claude} {
		pp := filepath.Join(dir, bindingKey(b)+".progress.json")
		data, err := os.ReadFile(pp)
		if err != nil {
			t.Fatal(err)
		}
		var saved relayProgress
		if err = json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.NextRetirementCheck.IsZero() {
			t.Fatal("probe schedule not persisted")
		}
		p := relayProgress{Run: b.Run, Thread: b.Thread, NextRetirementCheck: saved.NextRetirementCheck}
		if err = writePrivateJSON(pp, p); err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(filepath.Join(dir, bindingKey(b)+".activity.json")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	mu.Lock()
	counts = map[string]int{}
	mu.Unlock()
	if err := cmdRelay([]string{"--once"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	total = 0
	for _, n := range counts {
		total += n
	}
	mu.Unlock()
	t.Logf("TOTAL requests with saved probe schedule: %d", total)
	if total != 8 {
		t.Errorf("examined live pass=%d, want base8", total)
	}
	if _, err := os.Stat(filepath.Join(dir, bindingKey(codex)+".binding.json")); err != nil {
		t.Fatalf("live codex binding missing: %v", err)
	}
}

func TestRelayRetirementProbeBudgetSurvivesProgressReloads(t *testing.T) {
	for _, status := range []int{200, 429, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			dir, path, b, _, _ := retirementFixture(t)
			var reads atomic.Int32
			var servedRun atomic.Value
			servedRun.Store(b.Run)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if status != 200 {
					http.Error(w, "unavailable", status)
					return
				}
				json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, TaskID: b.Task, RunID: servedRun.Load().(string), Status: api.AgentRunning})
			}))
			defer server.Close()
			c, _ := api.NewClient(server.URL, time.Second)
			pp := filepath.Join(dir, bindingKey(b)+".progress.json")
			now := time.Date(2026, 9, 26, 18, 0, 0, 0, time.UTC)
			for i := 0; i < 20; i++ {
				// Reload for every pass, exercising the same persisted state as restarts.
				var p relayProgress
				data, err := os.ReadFile(pp)
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(data, &p); err != nil {
					t.Fatal(err)
				}
				retired, err := probeRelayRetirement(context.Background(), dir, path, b, &p, c, now.Add(time.Duration(i)*3*time.Second))
				if retired {
					t.Fatal("live binding retired")
				}
				if i == 0 && status != 200 && err == nil {
					t.Fatal("hub error hidden")
				}
				if err = saveRelayProgress(dir, pp, b, p); err != nil {
					t.Fatal(err)
				}
			}
			if reads.Load() != 1 {
				t.Fatalf("added reads during first minute=%d want1", reads.Load())
			}
			data, _ := os.ReadFile(pp)
			var p relayProgress
			json.Unmarshal(data, &p)
			probeRelayRetirement(context.Background(), dir, path, b, &p, c, now.Add(time.Minute))
			if reads.Load() != 2 {
				t.Fatalf("next-minute reads=%d want2", reads.Load())
			}
			// A successor gets its own immediate identity check, regardless of the
			// previous run's deadline; the old schedule cannot suppress migration.
			successor := b
			successor.Run = "run_0000000000000002"
			servedRun.Store(successor.Run)
			if err := writeRelayBinding(successor); err != nil {
				t.Fatal(err)
			}
			retired, err := probeRelayRetirement(context.Background(), dir, path, successor, &p, c, now.Add(time.Minute+time.Second))
			if retired || (status == 200 && err != nil) {
				t.Fatalf("live successor retired=%v err=%v", retired, err)
			}
			if reads.Load() != 3 {
				t.Fatalf("new run not immediately probed: %d", reads.Load())
			}
		})
	}
}
