package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTeamQueuePollBackoffKeepsRateLimitSpacing(t *testing.T) {
	now := time.Date(2026, time.September, 25, 20, 0, 0, 0, time.UTC)
	var backoff teamQueuePollBackoff
	rateLimited := &api.HTTPError{Status: http.StatusTooManyRequests, Msg: "synthetic 429"}
	backoff.observe(now, rateLimited)
	if backoff.ready(now.Add(5*time.Second)) || !backoff.ready(now.Add(6*time.Second)) {
		t.Fatalf("first 429 spacing: %+v", backoff)
	}
	backoff.observe(now.Add(6*time.Second), rateLimited)
	if backoff.ready(now.Add(17*time.Second)) || !backoff.ready(now.Add(18*time.Second)) {
		t.Fatalf("second 429 spacing: %+v", backoff)
	}
	backoff.observe(now.Add(18*time.Second), errors.New("other transient failure"))
	if backoff.delay != 12*time.Second {
		t.Fatalf("unrelated error erased 429 spacing: %+v", backoff)
	}
	backoff.observe(now.Add(18*time.Second), nil)
	if !backoff.ready(now.Add(18*time.Second)) || backoff.delay != 0 {
		t.Fatalf("successful poll did not reset spacing: %+v", backoff)
	}
}

func captureRelayOutput(t *testing.T, stderr bool, run func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var original *os.File
	if stderr {
		original, os.Stderr = os.Stderr, w
		defer func() { os.Stderr = original }()
	} else {
		original, os.Stdout = os.Stdout, w
		defer func() { os.Stdout = original }()
	}
	runErr := run()
	if stderr {
		os.Stderr = original
	} else {
		os.Stdout = original
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func TestRelayClearsRecoveredErrorAndLogsSameFailureAgain(t *testing.T) {
	// Keep the command's cleanup and queue tick away from the live hub, relay
	// state, config and tmux socket. No queue is due in this fixture.
	stateDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", stateDir)
	t.Setenv("TT_TMUX_SOCKET", "relay-recovery-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	queuePath := filepath.Join(stateDir, "fake-codex")
	if err := os.WriteFile(queuePath, []byte("#!/bin/sh\nprintf 'Queued message\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAILTERM_TOKEN", "")
	t.Setenv("TAILTERM_TASK", "")
	t.Setenv("TAILTERM_AGENT", "")
	t.Setenv("TAILTERM_RUN", "")
	var down atomic.Bool
	down.Store(true)
	b := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: queuePath}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "temporary outage", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/team-queues":
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case strings.HasSuffix(r.URL.Path, "/pause"):
			_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
		case strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/agents/"):
			_ = json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: true, Unread: 0})
		default:
			t.Errorf("unexpected test hub request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	t.Setenv("TAILTERM_HUB", server.URL)
	bindingPath := filepath.Join(stateDir, bindingKey(b)+".binding.json")
	progressPath := filepath.Join(stateDir, bindingKey(b)+".progress.json")
	if err := writePrivateJSON(bindingPath, b); err != nil {
		t.Fatal(err)
	}
	var bindingLogs string
	for _, phase := range []struct {
		name string
		down bool
	}{
		{"first failure", true}, {"repeat failure", true}, {"recovery", false}, {"same failure after recovery", true},
	} {
		down.Store(phase.down)
		out, err := captureRelayOutput(t, true, func() error { return cmdRelay([]string{"--once"}) })
		if err != nil {
			t.Fatalf("%s: %v", phase.name, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "[tt relay] "+b.Agent+": ") {
				bindingLogs += line + "\n"
			}
		}
		data, err := os.ReadFile(progressPath)
		if err != nil {
			t.Fatal(err)
		}
		var progress relayProgress
		if err := json.Unmarshal(data, &progress); err != nil {
			t.Fatal(err)
		}
		if phase.down && !strings.Contains(progress.Error, "temporary outage") {
			t.Fatalf("%s: saved error = %q", phase.name, progress.Error)
		}
		if !phase.down && progress.Error != "" {
			t.Fatalf("%s: saved error = %q, want empty", phase.name, progress.Error)
		}
		status, err := captureRelayOutput(t, false, func() error { return cmdRelay([]string{"--status"}) })
		if err != nil || strings.Contains(status, "temporary outage") != phase.down || !strings.Contains(status, b.Agent) {
			t.Fatalf("status after %s: %q, err=%v", phase.name, status, err)
		}
	}
	if got := strings.Count(bindingLogs, "[tt relay] "+b.Agent+": "); got != 2 {
		t.Fatalf("binding error log entries = %d, want 2: %s", got, bindingLogs)
	}
}

func TestRelayClearsErrorOnOtherSuccessfulPaths(t *testing.T) {
	for _, path := range []string{"paused", "inactive", "broker wake"} {
		t.Run(path, func(t *testing.T) {
			stateDir := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TAILTERM_RELAY_STATE", stateDir)
			t.Setenv("TT_TMUX_SOCKET", "relay-success-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
			t.Setenv("TAILTERM_TOKEN", "")
			t.Setenv("TAILTERM_TASK", "")
			t.Setenv("TAILTERM_AGENT", "")
			t.Setenv("TAILTERM_RUN", "")
			queueLog := filepath.Join(stateDir, "queue.log")
			t.Setenv("TT_FAKE_QUEUE_LOG", queueLog)
			queuePath := filepath.Join(stateDir, "fake-codex")
			if err := os.WriteFile(queuePath, []byte("#!/bin/sh\nprintf 'queued\\n' >> \"$TT_FAKE_QUEUE_LOG\"\nprintf 'Queued message\\n'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			b := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: queuePath}
			var reports atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/v1/team-queues":
					_, _ = w.Write([]byte(`{"entries":[]}`))
				case strings.HasSuffix(r.URL.Path, "/pause"):
					state := api.ProjectPauseActive
					if path == "paused" {
						state = api.ProjectPausePaused
					}
					_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: state})
				case strings.HasSuffix(r.URL.Path, "/wake-jobs/lease") && path == "broker wake":
					_ = json.NewEncoder(w).Encode(api.WakeJob{ID: "wake_1", LeaseToken: "test-lease", AgentID: b.Agent, RunID: b.Run, Prompt: "test broker prompt"})
				case strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
					http.NotFound(w, r)
				case strings.HasSuffix(r.URL.Path, "/wake-jobs/wake_1/report") && path == "broker wake":
					var report api.WakeJobReport
					if err := json.NewDecoder(r.Body).Decode(&report); err != nil || report.Status != "accepted" || report.LeaseToken != "test-lease" {
						t.Errorf("broker report = %+v, err=%v", report, err)
					}
					reports.Add(1)
					_, _ = w.Write([]byte(`{}`))
				case strings.Contains(r.URL.Path, "/agents/") && path != "paused":
					_ = json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: path != "inactive", Unread: 0})
				default:
					t.Errorf("unexpected test hub request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			b.Hub = server.URL
			t.Setenv("TAILTERM_HUB", server.URL)
			progressPath := filepath.Join(stateDir, bindingKey(b)+".progress.json")
			if err := writePrivateJSON(filepath.Join(stateDir, bindingKey(b)+".binding.json"), b); err != nil {
				t.Fatal(err)
			}
			if err := writePrivateJSON(progressPath, relayProgress{Run: b.Run, Thread: b.Thread, Error: "previous outage"}); err != nil {
				t.Fatal(err)
			}
			stderr, err := captureRelayOutput(t, true, func() error { return cmdRelay([]string{"--once"}) })
			if err != nil || strings.Contains(stderr, "[tt relay] "+b.Agent+": ") {
				t.Fatalf("%s pass: stderr=%q err=%v", path, stderr, err)
			}
			data, err := os.ReadFile(progressPath)
			if err != nil {
				t.Fatal(err)
			}
			var progress relayProgress
			if err := json.Unmarshal(data, &progress); err != nil || progress.Error != "" {
				t.Fatalf("%s saved progress: %+v err=%v", path, progress, err)
			}
			status, err := captureRelayOutput(t, false, func() error { return cmdRelay([]string{"--status"}) })
			if err != nil || strings.Contains(status, "previous outage") || !strings.Contains(status, b.Agent) {
				t.Fatalf("%s status: %q err=%v", path, status, err)
			}
			queueData, err := os.ReadFile(queueLog)
			if path == "broker wake" {
				if err != nil || string(queueData) != "queued\n" || reports.Load() != 1 || !progress.BrokerWakes {
					t.Fatalf("broker path: queue=%q err=%v reports=%d progress=%+v", queueData, err, reports.Load(), progress)
				}
			} else if !os.IsNotExist(err) || reports.Load() != 0 {
				t.Fatalf("%s unexpectedly queued: %q err=%v reports=%d", path, queueData, err, reports.Load())
			}
		})
	}
}

func TestRelayTargetsUnreadOnceAndPreservesReadReceipts(t *testing.T) {
	b := runtimeBinding{Hub: "http://hub", Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/usr/local/bin/codex"}
	a := api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: true, Unread: 1}
	messages := []api.Message{{Seq: 8, To: b.Agent, From: api.Sender{AgentID: "agt_0000000000000002"}, Text: "$(touch /tmp/should-never-run)"}}
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("relay must not mutate hub/read receipts: %s", r.Method)
			http.Error(w, "unexpected", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/messages") {
			after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
			var result []api.Message
			for _, m := range messages {
				if m.Seq > after {
					result = append(result, m)
				}
			}
			json.NewEncoder(w).Encode(api.MessageList{Messages: result})
		} else {
			reads++
			json.NewEncoder(w).Encode(a)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	c, _ := api.NewClient(server.URL, time.Second)
	p := relayProgress{}
	now := time.Now()
	calls := 0
	queue := func(ctx context.Context, got runtimeBinding, prompt string) error {
		calls++
		if got.Thread != b.Thread || !strings.Contains(prompt, "through message #8") || strings.Contains(prompt, "touch /tmp") {
			t.Fatalf("misdirected/raw payload: %+v %s", got, prompt)
		}
		return nil
	}
	if err := relayOne(context.Background(), b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || p.Through != 8 || a.ReadUpTo != 0 {
		t.Fatalf("delivery: %d %+v", calls, p)
	}
	// Persist/reload progress while the same message remains unread.
	raw, _ := json.Marshal(p)
	p = relayProgress{}
	json.Unmarshal(raw, &p)
	if err := relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("same unread message woke twice")
	}
	for _, status := range []string{api.AgentClosed, api.AgentExited, api.AgentRetired} {
		a.Status = status
		p.Through = 0
		relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue)
	}
	a.Status = api.AgentDone
	a.RunID = "run_0000000000000002"
	relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue)
	a.RunID = b.Run
	a.Online = false
	relayOne(context.Background(), b, &p, c, now.Add(time.Minute), queue)
	if calls != 1 || reads == 0 {
		t.Fatal("inactive/stale agent was resumed")
	}
	a.Online = true
	failed := func(context.Context, runtimeBinding, string) error { return errors.New("offline") }
	if err := relayOne(context.Background(), b, &p, c, now.Add(2*time.Minute), failed); err == nil || p.Through != 0 {
		t.Fatal("failed queue advanced cursor")
	}
	if err := relayOne(context.Background(), b, &p, c, now.Add(3*time.Minute), queue); err != nil || calls != 2 {
		t.Fatalf("retry: %v %d", err, calls)
	}
}

func TestRelayQueuesResumedRunAndRejectsStaleBinding(t *testing.T) {
	b := runtimeBinding{Hub: "http://hub", Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/usr/local/bin/codex"}
	a := api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: true, Unread: 1}
	messages := []api.Message{{Seq: 12, To: b.Agent, From: api.Sender{}, Text: "Human follow-up"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/messages") {
			json.NewEncoder(w).Encode(api.MessageList{Messages: messages})
			return
		}
		json.NewEncoder(w).Encode(a)
	}))
	defer server.Close()
	b.Hub = server.URL
	c, err := api.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	queue := func(_ context.Context, got runtimeBinding, prompt string) error {
		queued++
		if got.Agent != b.Agent || got.Run != b.Run || got.Thread != b.Thread || !strings.Contains(prompt, "through message #12") {
			t.Fatalf("wrong resumed binding/prompt: %+v %q", got, prompt)
		}
		return nil
	}
	if err := relayOne(context.Background(), b, &relayProgress{}, c, time.Now(), queue); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("resumed run queue calls = %d, want 1", queued)
	}
	stale := b
	stale.Run = "run_0000000000000002"
	if err := relayOne(context.Background(), stale, &relayProgress{}, c, time.Now(), queue); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("stale run binding queued: %d calls", queued)
	}
}

func TestRelayDoesNotWakeAcrossProjectPauseBarrier(t *testing.T) {
	b := runtimeBinding{Hub: "http://hub", Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/usr/local/bin/codex"}
	state := api.ProjectPauseCleanupPending
	a := api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: true, Unread: 1}
	messages := []api.Message{{Seq: 22, To: b.Agent, From: api.Sender{}, Text: "Owner follow-up while project is paused"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pause"):
			json.NewEncoder(w).Encode(api.ProjectPauseStatus{Version: 1, TaskID: b.Task, State: state, LifecycleGeneration: 1, PauseGeneration: 1})
		case strings.Contains(r.URL.Path, "/messages"):
			json.NewEncoder(w).Encode(api.MessageList{Messages: messages})
		default:
			json.NewEncoder(w).Encode(a)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	c, err := api.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	queue := func(context.Context, runtimeBinding, string) error { queued++; return nil }
	progress := relayProgress{}
	if err = relayOne(context.Background(), b, &progress, c, time.Now(), queue); err != nil || queued != 0 || progress.Through != 0 {
		t.Fatalf("cleanup-pending project woke: queued=%d progress=%+v err=%v", queued, progress, err)
	}
	state = api.ProjectPausePaused
	if err = relayOne(context.Background(), b, &progress, c, time.Now().Add(time.Minute), queue); err != nil || queued != 0 {
		t.Fatalf("fully paused project woke: queued=%d err=%v", queued, err)
	}
	state = api.ProjectPauseActive
	if err = relayOne(context.Background(), b, &progress, c, time.Now().Add(2*time.Minute), queue); err != nil || queued != 1 || progress.Through != 22 {
		t.Fatalf("explicitly resumed project did not wake normally: queued=%d progress=%+v err=%v", queued, progress, err)
	}
}

func TestWakeEligibility(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"other", "self", true}, {"", "self", true}, {"", "", true}, {"other", "", false}, {"self", "self", false}, {"other", "someone-else", false},
	} {
		through, wake := wakeThrough([]api.Message{{Seq: 10, From: api.Sender{AgentID: tc.from}, To: tc.to}}, "self")
		if through != 10 || wake != tc.want {
			t.Fatalf("%+v -> %d %v", tc, through, wake)
		}
	}
}

func TestSwarmWakePreservesOwnershipAndExcludesSelf(t *testing.T) {
	for _, c := range []struct {
		m    api.Message
		want bool
	}{
		{api.Message{Seq: 1, To: "worker", From: api.Sender{AgentID: "lead"}, Broadcast: true}, true},
		{api.Message{Seq: 2, From: api.Sender{AgentID: "lead"}, Broadcast: true}, true},
		{api.Message{Seq: 3, From: api.Sender{AgentID: "peer"}, Broadcast: true}, false},
		{api.Message{Seq: 4, To: "worker", From: api.Sender{AgentID: "lead"}}, false},
		{api.Message{Seq: 5, From: api.Sender{AgentID: "lead"}}, false},
	} {
		_, wake := wakeThrough([]api.Message{c.m}, "peer")
		if wake != c.want {
			t.Fatalf("%+v: %v", c.m, wake)
		}
	}
	task := api.Task{Name: "Swarm", Swarm: true, Orchestrator: "lead"}
	if got := taskBriefing(task, "worker", ""); !strings.Contains(got, "FIRST ORDER OF BUSINESS") || !strings.Contains(got, "tt post --to lead") || !strings.Contains(got, "SWARM ENABLED") {
		t.Fatal(got)
	}
	if got := taskBriefing(task, "lead", ""); !strings.Contains(got, "MAIN ORCHESTRATOR") || strings.Contains(got, "tt post --to lead") {
		t.Fatal(got)
	}
	// Phase 3.1 round one (B1): the briefing teaches tt ack and never says
	// not to acknowledge.
	for _, name := range []string{"worker", "lead"} {
		if got := strings.ToLower(taskBriefing(task, name, "")); !strings.Contains(got, "tt ack seq") || strings.Contains(got, "not acknowledge") {
			t.Fatalf("%s briefing: %s", name, got)
		}
	}
}
