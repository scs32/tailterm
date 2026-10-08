package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestClaudeWakePromptAndStableOutcome(t *testing.T) {
	messages := []api.Message{
		{Seq: 11, From: api.Sender{AgentID: "worker"}, Broadcast: true},
		{Seq: 12, From: api.Sender{AgentID: "lead"}, To: "worker"},
		{Seq: 13, From: api.Sender{AgentID: "lead"}, Broadcast: true},
	}
	prompt := claudeWakePrompt(messages, "worker")
	if strings.Contains(prompt, "#11") || !strings.Contains(prompt, "#12") || !strings.Contains(prompt, "#13") || !strings.Contains(prompt, "tt inbox") {
		t.Fatalf("wrong pending sequences: %q", prompt)
	}
	if got := claudeBrokerPrompt("Tailterm broker: #18 assign; #19 review. Run tt obligations.", 19, "wake_fixture"); !strings.Contains(got, "#18,#19") || !strings.Contains(got, "tt inbox") || !strings.Contains(got, "wake_fixture") {
		t.Fatalf("broker prompt: %q", got)
	}
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	var p relayProgress
	recordClaudeWake(&p, prompt, errors.New("Claude input is occupied"), now)
	if p.Wake == nil || p.Wake.Status != "skipped" || len(p.Wake.MessageSeqs) != 2 {
		t.Fatalf("skip outcome: %+v", p.Wake)
	}
	recordClaudeWake(&p, prompt, errors.New("Claude input is occupied"), now.Add(time.Minute))
	if !p.Wake.At.Equal(now) {
		t.Fatalf("steady skip changed timestamp: %+v", p.Wake)
	}
	recordClaudeWake(&p, prompt, nil, now.Add(time.Minute))
	if p.Wake.Status != "confirmed" || !p.Wake.At.Equal(now.Add(time.Minute)) {
		t.Fatalf("confirmed outcome: %+v", p.Wake)
	}
}

func TestClaudeWakeRejectsIneligibleHubRuns(t *testing.T) {
	b := testClaudeBinding()
	for _, tc := range []struct {
		name, run, status, pause string
		online                   bool
	}{
		{"stale run", "run_0000000000000002", api.AgentRunning, api.ProjectPauseActive, true},
		{"retired", b.Run, api.AgentRetired, api.ProjectPauseActive, true},
		// needs_input alone no longer refuses (wi_9201e1f3901d6fdc); every other
		// gate still does while the agent waits for input.
		{"stale run needing input", "run_0000000000000002", api.AgentNeedsInput, api.ProjectPauseActive, true},
		{"offline needing input", b.Run, api.AgentNeedsInput, api.ProjectPauseActive, false},
		{"paused needing input", b.Run, api.AgentNeedsInput, "paused", true},
		{"closed", b.Run, api.AgentClosed, api.ProjectPauseActive, true},
		{"offline", b.Run, api.AgentRunning, api.ProjectPauseActive, false},
		{"paused", b.Run, api.AgentRunning, "paused", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/pause"):
					_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: tc.pause})
				case strings.Contains(r.URL.Path, "/agents/") && !strings.Contains(r.URL.Path, "/wake-jobs"):
					_ = json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, RunID: tc.run, Status: tc.status, Online: tc.online, Runtime: "claude", Session: b.Session})
				default:
					t.Fatalf("ineligible wake made %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			binding := b
			binding.Hub = server.URL
			client, _ := api.NewClient(server.URL, time.Second)
			queued := 0
			queue := func(context.Context, runtimeBinding, string) error { queued++; return nil }
			if handled, err := relayWakeJob(context.Background(), binding, &relayProgress{}, client, time.Now(), queue); err != nil || handled {
				t.Fatalf("broker ineligible: %v %v", handled, err)
			}
			if err := relayOne(context.Background(), binding, &relayProgress{}, client, time.Now(), queue); err != nil {
				t.Fatalf("inbox ineligible: %v", err)
			}
			if queued != 0 {
				t.Fatalf("queued %d ineligible wakes", queued)
			}
		})
	}
}

func captureRelayOutput(t *testing.T, stderr bool, run func() error) (string, error) {
	t.Helper()
	if stderr {
		return captureStream(t, &os.Stderr, run)
	}
	return captureStream(t, &os.Stdout, run)
}

func TestCaptureRelayOutputReturnsOutputLargerThanAPipeBuffer(t *testing.T) {
	var want strings.Builder
	for i := 0; want.Len() <= 1<<20; i++ {
		fmt.Fprintf(&want, "line %07d of relay output larger than one pipe buffer\n", i)
	}
	for _, stderr := range []bool{false, true} {
		name := "stdout"
		if stderr {
			name = "stderr"
		}
		t.Run(name, func(t *testing.T) {
			oldStdout, oldStderr := os.Stdout, os.Stderr
			failure := errors.New("command failed after printing")
			got, err := captureRelayOutput(t, stderr, func() error {
				stream := os.Stdout
				if stderr {
					stream = os.Stderr
				}
				if _, err := stream.WriteString(want.String()); err != nil {
					return err
				}
				return failure
			})
			if err != failure {
				t.Fatalf("error = %v, want the command's own error", err)
			}
			if os.Stdout != oldStdout || os.Stderr != oldStderr {
				t.Fatal("os.Stdout or os.Stderr was not restored")
			}
			if got != want.String() {
				t.Fatalf("captured %d bytes, want %d bytes returned intact", len(got), want.Len())
			}
		})
	}
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
	var capabilityReads atomic.Int32
	b := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: queuePath}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "temporary outage", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/capabilities":
			capabilityReads.Add(1)
			http.NotFound(w, r) // This fixture models a hub without usage support.
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
	if got := capabilityReads.Load(); got != 1 {
		t.Fatalf("capability discovery requests = %d, want 1 after recovery", got)
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
			var capabilityReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/v1/capabilities":
					capabilityReads.Add(1)
					http.NotFound(w, r) // This fixture models a hub without usage support.
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
				case strings.Contains(r.URL.Path, "/agents/"):
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
			if got := capabilityReads.Load(); got != 1 {
				t.Fatalf("%s capability discovery requests = %d, want 1", path, got)
			}
		})
	}
}

func TestRelayActivityFailureLeavesQueueAndInboxDeliveryWorking(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", stateDir)
	t.Setenv("TT_TMUX_SOCKET", "relay-activity-isolation-"+strconv.FormatInt(time.Now().UnixNano(), 10))
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
	b := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: queuePath, CreatedAt: time.Now().UTC()}
	var teamTicks, agentReads, capabilityReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/capabilities":
			capabilityReads.Add(1)
			http.NotFound(w, r) // This fixture models a hub without usage support.
		case r.URL.Path == "/v1/team-queues":
			teamTicks.Add(1)
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case strings.HasSuffix(r.URL.Path, "/pause"):
			_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
		case strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_ = json.NewEncoder(w).Encode(api.MessageList{Messages: []api.Message{{Seq: 1, To: b.Agent, From: api.Sender{Node: "fixture", User: "owner"}, Text: "synthetic"}}})
		case strings.Contains(r.URL.Path, "/agents/"):
			// Retirement adds one identity read before the three delivery reads.
			if agentReads.Add(1) > 4 {
				http.Error(w, "synthetic observer failure", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentRunning, Online: true, Unread: 1})
		default:
			t.Errorf("unexpected test hub request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	t.Setenv("TAILTERM_HUB", server.URL)
	if err := writePrivateJSON(filepath.Join(stateDir, bindingKey(b)+".binding.json"), b); err != nil {
		t.Fatal(err)
	}
	stderr, err := captureRelayOutput(t, true, func() error { return cmdRelay([]string{"--once"}) })
	if err != nil || !strings.Contains(stderr, "activity:") || teamTicks.Load() != 1 || agentReads.Load() < 5 {
		t.Fatalf("relay isolation: ticks=%d reads=%d stderr=%q err=%v", teamTicks.Load(), agentReads.Load(), stderr, err)
	}
	if data, err := os.ReadFile(queueLog); err != nil || !strings.Contains(string(data), "queued") {
		t.Fatalf("inbox delivery lost: %q %v stderr=%q", data, err, stderr)
	}
	if got := capabilityReads.Load(); got != 1 {
		t.Fatalf("capability discovery requests = %d, want 1", got)
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

// needsInputHub is an isolated hub fixture for the relay's Claude paths. It
// answers only the reads and broker writes the relay already makes and fails
// the test on any other call.
type needsInputHub struct {
	t        *testing.T
	mu       sync.Mutex
	agent    api.Agent
	paused   bool
	messages []api.Message
	jobs     []api.WakeJob
	reports  []api.WakeJobReport
	calls    []string
}

func (h *needsInputHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	h.calls = append(h.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pause"):
		state := api.ProjectPauseActive
		if h.paused {
			state = "paused"
		}
		_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: state})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
		job := api.WakeJob{}
		if len(h.jobs) > 0 {
			job, h.jobs = h.jobs[0], h.jobs[1:]
		}
		_ = json.NewEncoder(w).Encode(job)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/wake-jobs/") && strings.HasSuffix(r.URL.Path, "/report"):
		var report api.WakeJobReport
		_ = json.NewDecoder(r.Body).Decode(&report)
		h.reports = append(h.reports, report)
		_, _ = w.Write([]byte("{}"))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/obligations"):
		_ = json.NewEncoder(w).Encode(api.ObligationList{})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/messages"):
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		var page []api.Message
		for _, m := range h.messages {
			if m.Seq > after {
				page = append(page, m)
			}
		}
		_ = json.NewEncoder(w).Encode(api.MessageList{Messages: page})
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/agents/"):
		_ = json.NewEncoder(w).Encode(h.agent)
	default:
		h.t.Errorf("unexpected hub call %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

func (h *needsInputHub) update(change func(*needsInputHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change(h)
}

func (h *needsInputHub) takeCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	calls := h.calls
	h.calls = nil
	return calls
}

// fakeClaudePane runs the real claudeWakeWith guard and claudeInputScreen
// against a captured pane and a synthetic transcript. Enter appends the typed
// text as a new user record, as Claude Code does.
type fakeClaudePane struct {
	transcript       string
	raw              string
	cursorX, cursorY int
	busy             bool
	typed            string
	sent             []string
}

const fakeClaudeRule = "\x1b[38;5;244m────────────────────────────────────────"

func newFakeClaudePane(t *testing.T) *fakeClaudePane {
	transcript := filepath.Join(t.TempDir(), "claude.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"stop_reason":"end_turn","content":[]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &fakeClaudePane{transcript: transcript, raw: "Posted the decision request.\n" + fakeClaudeRule + "\n\x1b[39m❯ \n" + fakeClaudeRule + "\n\x1b[39m  ⏵⏵ bypass permissions on\n", cursorX: 2, cursorY: 2}
}

func (f *fakeClaudePane) inspect(_ context.Context, _ runtimeBinding, expected string) (claudeWakeSnapshot, error) {
	raw := f.raw
	if expected != "" {
		raw = "\x1b[39m❯ " + f.typed + "\n" + fakeClaudeRule + "\n"
	}
	screen, err := claudeInputScreen(raw, f.cursorX, f.cursorY, expected)
	if err != nil {
		return claudeWakeSnapshot{}, err
	}
	info, err := os.Stat(f.transcript)
	if err != nil {
		return claudeWakeSnapshot{}, err
	}
	return claudeWakeSnapshot{Pane: "%1", SessionID: "$1", Created: "100", PanePID: 1001, Path: f.transcript, FileID: fileIdentity(info), Offset: info.Size(), Screen: screen,
		Cursor: activityCursor{Ready: true, SeenTurn: true, TurnComplete: !f.busy}}, nil
}

func (f *fakeClaudePane) send(_ context.Context, pane, value string, literal bool) error {
	if literal {
		f.typed = value
		f.sent = append(f.sent, value)
		return nil
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": f.typed}})
	file, err := os.OpenFile(f.transcript, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	f.typed = ""
	_, err = file.Write(append(line, '\n'))
	return err
}

func (f *fakeClaudePane) queue(ctx context.Context, b runtimeBinding, prompt string) error {
	return claudeWakeWith(ctx, b, prompt, claudeWakeOps{inspect: f.inspect, send: f.send, sleep: func(time.Duration) {}, now: time.Now})
}

// relayPass mirrors one cmdRelay binding pass: the broker path first, and the
// inbox path when the broker path did not queue.
func relayPass(b runtimeBinding, p *relayProgress, c *api.Client, now time.Time, queue func(context.Context, runtimeBinding, string) error) error {
	queued, _ := relayWakeJob(context.Background(), b, p, c, now, queue)
	if queued {
		return nil
	}
	return relayOne(context.Background(), b, p, c, now, queue)
}

func needsInputFixture(t *testing.T) (runtimeBinding, *needsInputHub, *api.Client, *fakeClaudePane) {
	t.Helper()
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	b := testClaudeBinding()
	hub := &needsInputHub{t: t, agent: api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentNeedsInput, Online: true, Runtime: "claude", Session: b.Session}}
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	b.Hub = server.URL
	c, err := api.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return b, hub, c, newFakeClaudePane(t)
}

const needsInputHandler = "agt_00000000000000aa"

// k1: an agent in needs_input is woken by an answer to its decision, by a
// directed message and by a broker job, each once; a later message wakes once
// more.
func TestClaudeWakeNeedsInputWakesForNewInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input api.Message
		job   bool
		want  string
	}{
		{"decision answer", api.Message{Seq: 102, To: "self", From: api.Sender{Node: "workspace", User: "owner"}, ReplyTo: 101, DecisionAnswer: &api.DecisionAnswer{}, Text: "Option A"}, false, "Tailterm messages #102. Run tt inbox --unread --mark-read."},
		{"directed message", api.Message{Seq: 102, To: "self", From: api.Sender{AgentID: needsInputHandler}, Text: "Start saved"}, false, "Tailterm messages #102. Run tt inbox --unread --mark-read."},
		{"broker job", api.Message{Seq: 102, To: "self", From: api.Sender{AgentID: needsInputHandler}, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Handler notice for the waiting lead"}}, true, "Tailterm obligations #102. Run tt inbox --unread --mark-read. Wake wake_0123456789abcdef."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, hub, c, pane := needsInputFixture(t)
			input := tc.input
			input.To = b.Agent
			hub.update(func(h *needsInputHub) {
				h.agent.ReadUpTo, h.agent.Unread = 101, 1
				h.messages = []api.Message{{Seq: 101, From: api.Sender{AgentID: b.Agent}, Text: "Decision request"}, input}
				if tc.job {
					h.jobs = []api.WakeJob{{ID: "wake_0123456789abcdef", LeaseToken: "lease", MessageSeq: 102, AgentID: b.Agent, RunID: b.Run, Prompt: "Tailterm broker: #102 notice."}}
				}
			})
			var p relayProgress
			now := time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC)
			if err := relayPass(b, &p, c, now, pane.queue); err != nil {
				t.Fatal(err)
			}
			if len(pane.sent) != 1 || pane.sent[0] != tc.want || p.Wake == nil || p.Wake.Status != "confirmed" || p.Skip != nil {
				t.Fatalf("needs_input agent was not woken once: sent=%q wake=%+v skip=%+v", pane.sent, p.Wake, p.Skip)
			}
			if tc.job && (len(hub.reports) != 1 || hub.reports[0].Status != "accepted") {
				t.Fatalf("broker report: %+v", hub.reports)
			}
			// k1/a2: input already delivered is not woken again.
			for _, later := range []time.Duration{20 * time.Second, 40 * time.Second} {
				if err := relayPass(b, &p, c, now.Add(later), pane.queue); err != nil {
					t.Fatal(err)
				}
			}
			if len(pane.sent) != 1 {
				t.Fatalf("repeat wake for delivered input: %q", pane.sent)
			}
			hub.update(func(h *needsInputHub) {
				h.agent.Unread = 2
				h.messages = append(h.messages, api.Message{Seq: 105, To: b.Agent, From: api.Sender{AgentID: needsInputHandler}, Text: "Follow-up"})
			})
			for _, later := range []time.Duration{time.Minute, 80 * time.Second} {
				if err := relayPass(b, &p, c, now.Add(later), pane.queue); err != nil {
					t.Fatal(err)
				}
			}
			if len(pane.sent) != 2 || pane.sent[1] != "Tailterm messages #105. Run tt inbox --unread --mark-read." {
				t.Fatalf("new later message did not wake exactly once: %q", pane.sent)
			}
		})
	}
}

// k2: replay of the 2026-09-28 stall. lead-52bec739 posted decision #13896,
// ran tt event needs_input and finished its turn; the owner's answer #13897
// and the handler's directed #13899 and #13901 then arrived and never woke it.
func TestClaudeWakeNeedsInputIncidentReplay(t *testing.T) {
	incident := func(agent string) []api.Message {
		return []api.Message{
			{Seq: 13896, From: api.Sender{AgentID: agent}, DecisionRequest: &api.DecisionRequest{}, Text: "Decision request"},
			{Seq: 13897, To: agent, From: api.Sender{Node: "workspace", User: "owner"}, ReplyTo: 13896, DecisionAnswer: &api.DecisionAnswer{}, Text: "Answer"},
			{Seq: 13899, To: agent, From: api.Sender{AgentID: needsInputHandler}, Envelope: &api.Envelope{Kind: api.EnvelopeKindResult, Subject: "Handler saved the requested record"}},
			{Seq: 13901, To: agent, From: api.Sender{AgentID: needsInputHandler}, Text: "Handler follow-up"},
		}
	}
	now := time.Date(2026, 9, 28, 15, 59, 0, 0, time.UTC)
	t.Run("wakes", func(t *testing.T) {
		b, hub, c, pane := needsInputFixture(t)
		// The lead is mid-turn while it posts the decision request.
		pane.busy = true
		hub.update(func(h *needsInputHub) {
			h.agent.Status, h.agent.ReadUpTo = api.AgentRunning, 13896
			h.messages = incident(b.Agent)[:1]
		})
		var p relayProgress
		if err := relayPass(b, &p, c, now, pane.queue); err != nil {
			t.Fatal(err)
		}
		// tt event needs_input, turn complete, then the answer and messages.
		pane.busy = false
		hub.update(func(h *needsInputHub) {
			h.agent.Status, h.agent.Unread = api.AgentNeedsInput, 3
			h.messages = incident(b.Agent)
		})
		for _, later := range []time.Duration{20 * time.Second, 40 * time.Second, 60 * time.Second} {
			if err := relayPass(b, &p, c, now.Add(later), pane.queue); err != nil {
				t.Fatal(err)
			}
		}
		want := "Tailterm messages #13897,#13899,#13901. Run tt inbox --unread --mark-read."
		if len(pane.sent) != 1 || pane.sent[0] != want {
			t.Fatalf("incident replay did not wake exactly once: %q", pane.sent)
		}
		if p.Wake == nil || p.Wake.Status != "confirmed" || fmt.Sprint(p.Wake.MessageSeqs) != "[13897 13899 13901]" || p.Through != 13901 {
			t.Fatalf("wake outcome: %+v through=%d", p.Wake, p.Through)
		}
	})
	for _, tc := range []struct {
		name, reason string
		setup        func(*needsInputHub, *fakeClaudePane)
		listed       bool
	}{
		{"busy transcript", "not safely idle", func(_ *needsInputHub, pane *fakeClaudePane) { pane.busy = true }, true},
		{"permission dialog", "permission or selection prompt", func(_ *needsInputHub, pane *fakeClaudePane) {
			pane.raw = "\x1b[1mBash command\x1b[0m\n  rm -rf build\nDo you want to proceed?\n\x1b[39m❯ 1. Yes\n  2. No\n\nEsc to cancel\n"
			pane.cursorY = 3
		}, true},
		{"wrong run", "run superseded", func(h *needsInputHub, _ *fakeClaudePane) { h.agent.RunID = "run_0000000000000002" }, false},
	} {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			b, hub, c, pane := needsInputFixture(t)
			hub.update(func(h *needsInputHub) {
				h.agent.ReadUpTo, h.agent.Unread = 13896, 3
				h.messages = incident(b.Agent)
				tc.setup(h, pane)
			})
			var p relayProgress
			_, err := captureRelayOutput(t, true, func() error { return relayPass(b, &p, c, now, pane.queue) })
			if len(pane.sent) != 0 {
				t.Fatalf("%s with needs_input typed input: %q (%v)", tc.name, pane.sent, err)
			}
			if p.Skip == nil || !strings.Contains(p.Skip.Reason, tc.reason) || p.Through != 0 {
				t.Fatalf("%s skip: %+v through=%d err=%v", tc.name, p.Skip, p.Through, err)
			}
			if tc.listed && fmt.Sprint(p.Skip.MessageSeqs) != "[13897 13899 13901]" {
				t.Fatalf("%s skip seqs: %+v", tc.name, p.Skip)
			}
			if !tc.listed && (p.Skip.After != 13896 || p.Skip.Unread != 3) {
				t.Fatalf("%s skip cursor: %+v", tc.name, p.Skip)
			}
			for _, call := range hub.takeCalls() {
				if !tc.listed && strings.HasSuffix(call, "/messages") {
					t.Fatalf("wrong run listed messages: %s", call)
				}
			}
		})
	}
}

// k3: each held-back wake records its reason with the agent and sequences,
// logs once per change with an RFC3339 time, and adds no hub calls.
func TestRelaySkipRecordsReasonAndSeqsOnce(t *testing.T) {
	b, hub, c, pane := needsInputFixture(t)
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC)
	line := func(want string, run func() error) {
		t.Helper()
		out, _ := captureRelayOutput(t, true, run)
		if want == "" {
			if out != "" {
				t.Fatalf("repeated skip logged again: %q", out)
			}
			return
		}
		if !strings.HasPrefix(out, "[tt relay] 2026-09-28T16:") || !strings.Contains(out, " "+b.Agent+" wake skipped: ") || !strings.HasSuffix(strings.TrimSpace(out), want) || strings.Count(out, "\n") != 1 {
			t.Fatalf("skip log %q, want suffix %q", out, want)
		}
	}
	var p relayProgress
	hub.update(func(h *needsInputHub) {
		h.agent.Online, h.agent.ReadUpTo, h.agent.Unread = false, 40, 2
		h.messages = []api.Message{{Seq: 41, To: b.Agent, From: api.Sender{AgentID: needsInputHandler}}, {Seq: 42, To: b.Agent, From: api.Sender{Node: "workspace", User: "owner"}}}
	})
	hub.takeCalls()
	line("agent offline after=#40 unread=2", func() error { return relayOne(context.Background(), b, &p, c, now, pane.queue) })
	if calls := hub.takeCalls(); len(calls) != 2 || !strings.HasSuffix(calls[0], "/pause") || !strings.Contains(calls[1], "/agents/") {
		t.Fatalf("skip recording changed hub calls: %v", calls)
	}
	first := p.Skip.At
	line("", func() error { return relayOne(context.Background(), b, &p, c, now.Add(30*time.Second), pane.queue) })
	if p.Skip == nil || !p.Skip.At.Equal(first) || p.Skip.Reason != "agent offline" {
		t.Fatalf("steady skip: %+v", p.Skip)
	}
	hub.update(func(h *needsInputHub) { h.paused = true })
	line("project not active after=#0 unread=0", func() error { return relayOne(context.Background(), b, &p, c, now.Add(time.Minute), pane.queue) })
	hub.update(func(h *needsInputHub) { h.paused, h.agent.Online = false, true })
	p.Wakes, p.Window = 8, now.Add(time.Minute)
	line("five-minute rate window resets after=#40 unread=2", func() error { return relayOne(context.Background(), b, &p, c, now.Add(2*time.Minute), pane.queue) })
	p.Wakes = 0
	pane.busy = true
	line("seqs=[41 42]", func() error { return relayOne(context.Background(), b, &p, c, now.Add(3*time.Minute), pane.queue) })
	if !strings.Contains(p.Skip.Reason, "not safely idle") || p.Wake == nil || p.Wake.Status != "skipped" {
		t.Fatalf("unsafe wake skip: %+v wake=%+v", p.Skip, p.Wake)
	}
	line("", func() error { return relayOne(context.Background(), b, &p, c, now.Add(4*time.Minute), pane.queue) })
	pane.busy = false
	line("", func() error { return relayOne(context.Background(), b, &p, c, now.Add(5*time.Minute), pane.queue) })
	if p.Skip != nil || len(pane.sent) != 1 || p.Wake.Status != "confirmed" {
		t.Fatalf("confirmed wake kept skip: %+v sent=%q", p.Skip, pane.sent)
	}
}

// k3: tt relay --status shows the latest skip from local progress only.
func TestRelaySkipStatusShowsLastSkip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", dir)
	b := testClaudeBinding()
	if err := writeRelayBinding(b); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 28, 16, 0, 2, 0, time.UTC)
	path := filepath.Join(dir, bindingKey(b)+".progress.json")
	for _, tc := range []struct {
		skip relaySkip
		want []string
	}{
		{relaySkip{Reason: "Claude is not safely idle", MessageSeqs: []int64{13897, 13899}, At: at}, []string{`skip="Claude is not safely idle"`, "skip-seqs=[13897 13899]", "skip-at=2026-09-28T16:00:02Z"}},
		{relaySkip{Reason: "agent offline", After: 13896, Unread: 3, At: at}, []string{`skip="agent offline"`, "skip-seqs=[]", "skip-at=2026-09-28T16:00:02Z", "skip-after=#13896 skip-unread=3"}},
	} {
		skip := tc.skip
		if err := saveRelayProgress(dir, path, b, relayProgress{Run: b.Run, Thread: b.Thread, Skip: &skip}); err != nil {
			t.Fatal(err)
		}
		out, err := captureRelayOutput(t, false, func() error { return cmdRelay([]string{"--status"}) })
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Fatalf("status %q missing %q", out, want)
			}
		}
	}
}

// k3 review f1: a message left to broker wake jobs is a deferral, not a skip.
// After a confirmed broker wake, and inside the broker spacing before a job
// is leased, the inbox path records and logs nothing.
func TestRelaySkipNotRecordedForBrokerDeferral(t *testing.T) {
	b, hub, c, pane := needsInputFixture(t)
	notice := func(seq int64) api.Message {
		return api.Message{Seq: seq, To: b.Agent, From: api.Sender{AgentID: needsInputHandler}, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Handler notice for the waiting lead"}}
	}
	hub.update(func(h *needsInputHub) {
		h.agent.ReadUpTo, h.agent.Unread = 101, 1
		h.messages = []api.Message{notice(102)}
		h.jobs = []api.WakeJob{{ID: "wake_0123456789abcdef", LeaseToken: "lease", MessageSeq: 102, AgentID: b.Agent, RunID: b.Run, Prompt: "Tailterm broker: #102 notice."}}
	})
	var p relayProgress
	now := time.Date(2026, 9, 28, 16, 0, 0, 0, time.UTC)
	pass := func(at time.Duration) string {
		t.Helper()
		out, err := captureRelayOutput(t, true, func() error { return relayPass(b, &p, c, now.Add(at), pane.queue) })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := pass(0); out != "" || len(pane.sent) != 1 || p.Wake == nil || p.Wake.Status != "confirmed" {
		t.Fatalf("broker wake: sent=%q wake=%+v log=%q", pane.sent, p.Wake, out)
	}
	// The reviewer's probe: the agent has not read #102 yet 3 and 20 seconds on.
	for _, at := range []time.Duration{3 * time.Second, 20 * time.Second} {
		if out := pass(at); out != "" || p.Skip != nil {
			t.Fatalf("false skip %v after a confirmed broker wake: %+v log=%q", at, p.Skip, out)
		}
	}
	// A new broker-covered message inside the 15-second broker spacing, before
	// its job is leased, is deferred without a skip.
	p.LastBrokerWake = now.Add(25 * time.Second)
	hub.update(func(h *needsInputHub) {
		h.agent.Unread = 2
		h.messages = append(h.messages, notice(103))
		h.jobs = []api.WakeJob{{ID: "wake_00000000000000ab", LeaseToken: "lease", MessageSeq: 103, AgentID: b.Agent, RunID: b.Run, Prompt: "Tailterm broker: #103 notice."}}
	})
	if out := pass(30 * time.Second); out != "" || p.Skip != nil || len(pane.sent) != 1 {
		t.Fatalf("broker spacing recorded a skip: %+v sent=%q log=%q", p.Skip, pane.sent, out)
	}
	if out := pass(45 * time.Second); len(pane.sent) != 2 || !strings.Contains(pane.sent[1], "#103") || out != "" {
		t.Fatalf("leased job after spacing: sent=%q log=%q", pane.sent, out)
	}
	// An unsafe broker attempt is still recorded with its sequence.
	pane.busy = true
	hub.update(func(h *needsInputHub) {
		h.agent.Unread = 3
		h.messages = append(h.messages, notice(104))
		h.jobs = []api.WakeJob{{ID: "wake_00000000000000ac", LeaseToken: "lease", MessageSeq: 104, AgentID: b.Agent, RunID: b.Run, Prompt: "Tailterm broker: #104 notice."}}
	})
	if out := pass(time.Minute + 5*time.Second); p.Skip == nil || fmt.Sprint(p.Skip.MessageSeqs) != "[104]" || !strings.Contains(p.Skip.Reason, "not safely idle") || !strings.Contains(out, "wake skipped") {
		t.Fatalf("unsafe broker wake skip: %+v log=%q", p.Skip, out)
	}
}

// wi_a31c079e98518efc (i1): an unlinked free-text message addressed to a
// work-item-bound agent counts as unread and wakes it through the relay, on a
// real test hub.
func TestRelayWakesBoundAgentForUnlinkedDirected(t *testing.T) {
	h := newInboxHub(t)
	ctx := context.Background()
	be, bound, _, _ := h.bind(t, "worker-bound")
	if _, err := h.st.PostEvent(ctx, h.task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: bound.ID, RunID: bound.RunID}, h.by); err != nil {
		t.Fatal(err)
	}
	directed := h.post(t, api.PostMessageRequest{Text: "unlinked lead note", AgentID: h.lead.ID, To: bound.ID}, 1)[0]
	b := runtimeBinding{Hub: be.hub, Task: be.task, Agent: bound.ID, Run: bound.RunID, Thread: "00000000-0000-4000-8000-000000000001", Codex: "/usr/local/bin/codex"}
	p := relayProgress{Run: b.Run, Thread: b.Thread}
	var prompts []string
	queue := func(_ context.Context, got runtimeBinding, prompt string) error {
		prompts = append(prompts, prompt)
		return nil
	}
	if err := relayOne(ctx, b, &p, h.c, time.Now(), queue); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || p.Through != directed.Seq || !strings.Contains(prompts[0], fmt.Sprintf("through message #%d", directed.Seq)) {
		t.Fatalf("relay did not wake the bound agent for #%d: prompts=%q progress=%+v", directed.Seq, prompts, p)
	}
}

func TestRelayCodexOwnerHelperWakeRoutes(t *testing.T) {
	for _, role := range []string{api.AgentRoleOwnerHelper, ""} {
		t.Run("role-"+role, func(t *testing.T) {
			h := &needsInputHub{t: t, agent: api.Agent{ID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", Runtime: "codex", Role: role, Status: api.AgentDone, Online: true, Unread: 1}}
			h.messages = []api.Message{{Seq: 41, To: h.agent.ID, From: api.Sender{Node: "workspace", User: "owner"}, Text: "owner input"}}
			h.jobs = []api.WakeJob{{ID: "wake_0123456789abcdef", LeaseToken: "lease", MessageSeq: 42, AgentID: h.agent.ID, RunID: h.agent.RunID, Prompt: "Tailterm broker: #42 request. Run tt obligations."}}
			hub := httptest.NewServer(h)
			defer hub.Close()
			c, _ := api.NewClient(hub.URL, time.Second)
			b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: h.agent.ID, Run: h.agent.RunID, Thread: "00000000-0000-4000-8000-000000000001", Runtime: "codex", Role: role, Codex: "/synthetic/codex"}
			var prompts []string
			queue := func(_ context.Context, got runtimeBinding, prompt string) error {
				if got != b {
					t.Fatal("queue binding changed")
				}
				prompts = append(prompts, prompt)
				return nil
			}
			if err := relayOne(context.Background(), b, &relayProgress{}, c, time.Now(), queue); err != nil {
				t.Fatal(err)
			}
			if handled, err := relayWakeJob(context.Background(), b, &relayProgress{}, c, time.Now(), queue); err != nil || !handled {
				t.Fatalf("broker handled=%v err=%v", handled, err)
			}
			if len(prompts) != 2 {
				t.Fatalf("prompts=%v", prompts)
			}
			for _, prompt := range prompts {
				if role == api.AgentRoleOwnerHelper && (!strings.Contains(prompt, "tt helper inbox --task "+b.Task) || strings.Contains(prompt, "tt obligations") || strings.Contains(prompt, "tt inbox --unread")) {
					t.Fatalf("helper prompt %q", prompt)
				}
				if role == "" && strings.Contains(prompt, "tt helper inbox") {
					t.Fatalf("ordinary Codex changed %q", prompt)
				}
			}
			if role == "" && prompts[1] != "Tailterm broker: #42 request. Run tt obligations." {
				t.Fatalf("ordinary broker prompt changed %q", prompts[1])
			}
			if len(h.reports) != 1 || h.reports[0].Status != "accepted" {
				t.Fatalf("broker reports %+v", h.reports)
			}
		})
	}
}

// rotationLoopFixture runs the relay loop against a local hub with one bound
// agent, away from the live hub, relay state, config and tmux socket. It
// counts the team queue polls and the bound agent's wake-pass reads.
type rotationLoopFixture struct {
	queuePolls, agentReads atomic.Int32
}

func newRotationLoopFixture(t *testing.T, handler, steward func(context.Context) error, pause func() bool) *rotationLoopFixture {
	t.Helper()
	f := &rotationLoopFixture{}
	stateDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", stateDir)
	t.Setenv("TT_TMUX_SOCKET", "relay-rotation-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	for _, name := range []string{"TAILTERM_TOKEN", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN"} {
		t.Setenv(name, "")
	}
	b := runtimeBinding{Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: filepath.Join(stateDir, "fake-codex")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/team-queues":
			f.queuePolls.Add(1)
			_, _ = w.Write([]byte(`{"entries":[]}`))
		case strings.HasSuffix(r.URL.Path, "/pause"):
			_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
		case strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/agents/"):
			f.agentReads.Add(1)
			_ = json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: true, Unread: 0})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	b.Hub = server.URL
	t.Setenv("TAILTERM_HUB", server.URL)
	if err := writePrivateJSON(filepath.Join(stateDir, bindingKey(b)+".binding.json"), b); err != nil {
		t.Fatal(err)
	}
	oldHandler, oldSteward, oldPause := relayHandlerRotation, relayStewardRotation, relayLoopPause
	t.Cleanup(func() { relayHandlerRotation, relayStewardRotation, relayLoopPause = oldHandler, oldSteward, oldPause })
	relayHandlerRotation, relayStewardRotation, relayLoopPause = handler, steward, pause
	return f
}

func TestRelayLoopWakesWhileRotationTickBlocks(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var mu sync.Mutex
	var order []string
	var enteredAt time.Time
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	// The handler tick holds its whole two-minute budget unless released.
	handler := func(ctx context.Context) error {
		record("handler")
		mu.Lock()
		first := enteredAt.IsZero()
		if first {
			enteredAt = time.Now()
		}
		mu.Unlock()
		if !first {
			return nil
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	steward := func(context.Context) error { record("steward"); return nil }
	var f *rotationLoopFixture
	var passes int
	var readsWhenBlocked, readsAfter int32
	var wakeDelay time.Duration
	pause := func() bool {
		passes++
		switch passes {
		case 1:
			// The pass started during the first loop pass; the tick now blocks.
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				t.Error("rotation tick never started")
				return false
			}
			readsWhenBlocked = f.agentReads.Load()
			return true
		case 2, 3:
			return true
		}
		// Three more full passes ran while the tick was still blocked.
		mu.Lock()
		wakeDelay = time.Since(enteredAt)
		mu.Unlock()
		readsAfter = f.agentReads.Load()
		mu.Lock()
		if got := strings.Join(order, ","); got != "handler" {
			t.Errorf("ticks while the handler tick blocks = %q, want one handler tick", got)
		}
		mu.Unlock()
		unblock()
		return false
	}
	f = newRotationLoopFixture(t, handler, steward, pause)
	if err := cmdRelay(nil); err != nil {
		t.Fatal(err)
	}
	if readsAfter <= readsWhenBlocked {
		t.Fatalf("no wake pass read the agent while rotation blocked: %d then %d", readsWhenBlocked, readsAfter)
	}
	if wakeDelay > 15*time.Second {
		t.Fatalf("wake passes took %s behind a blocked rotation tick, want at most 15s", wakeDelay)
	}
	if got := strings.Join(order, ","); got != "handler,steward" {
		t.Fatalf("rotation ticks = %q, want handler then steward once", got)
	}
}

func TestRelayLoopHoldsQueueAndRotationAfterRotationRateLimit(t *testing.T) {
	var handlerCalls, stewardCalls atomic.Int32
	handler := func(context.Context) error {
		handlerCalls.Add(1)
		return &api.HTTPError{Status: http.StatusTooManyRequests, Msg: "synthetic 429"}
	}
	steward := func(context.Context) error { stewardCalls.Add(1); return nil }
	var passes int
	pause := func() bool {
		passes++
		time.Sleep(50 * time.Millisecond)
		return passes < 12
	}
	f := newRotationLoopFixture(t, handler, steward, pause)
	stderr, err := captureRelayOutput(t, true, func() error { return cmdRelay(nil) })
	if err != nil {
		t.Fatal(err)
	}
	// Twelve passes fit well inside the first six-second spacing.
	if handlerCalls.Load() != 1 || stewardCalls.Load() != 0 {
		t.Fatalf("handler ticks = %d, steward ticks = %d, want 1 and 0", handlerCalls.Load(), stewardCalls.Load())
	}
	if got := f.queuePolls.Load(); got < 1 || got > 3 {
		t.Fatalf("team queue polls over %d passes = %d, want them held off after the 429", passes, got)
	}
	if got := strings.Count(stderr, "[tt relay] handler rotation: "); got != 1 || !strings.Contains(stderr, "synthetic 429") {
		t.Fatalf("handler rotation log entries = %d: %s", got, stderr)
	}
}

func TestRelayRotationPassObservesBackoffLikeInlineTicks(t *testing.T) {
	rateLimited := &api.HTTPError{Status: http.StatusTooManyRequests, Msg: "synthetic 429"}
	var handlerErr, stewardErr error
	var calls []string
	oldHandler, oldSteward := relayHandlerRotation, relayStewardRotation
	t.Cleanup(func() { relayHandlerRotation, relayStewardRotation = oldHandler, oldSteward })
	relayHandlerRotation = func(context.Context) error { calls = append(calls, "handler"); return handlerErr }
	relayStewardRotation = func(context.Context) error { calls = append(calls, "steward"); return stewardErr }
	var backoff teamQueuePollBackoff
	var rotation relayRotationPass
	// run mirrors the loop: a pass starts only when the backoff is ready.
	run := func() (string, string) {
		t.Helper()
		calls = nil
		stderr, _ := captureRelayOutput(t, true, func() error {
			if backoff.ready(time.Now()) {
				rotation.start()
			}
			rotation.collect(&backoff, true)
			return nil
		})
		return strings.Join(calls, ","), stderr
	}
	elapse := func() { backoff.next = time.Now().Add(-time.Second) }

	handlerErr = rateLimited
	if got, stderr := run(); got != "handler" || backoff.delay != 6*time.Second || backoff.ready(time.Now()) || !strings.Contains(stderr, "[tt relay] handler rotation: ") {
		t.Fatalf("handler 429: ticks=%q backoff=%+v stderr=%q", got, backoff, stderr)
	}
	if got, _ := run(); got != "" {
		t.Fatalf("rotation ran inside the backoff: %q", got)
	}
	elapse()
	if got, _ := run(); got != "handler" || backoff.delay != 12*time.Second || backoff.ready(time.Now()) {
		t.Fatalf("second handler 429: ticks=%q backoff=%+v", got, backoff)
	}
	backoff.delay = time.Minute
	elapse()
	if _, _ = run(); backoff.delay != time.Minute {
		t.Fatalf("backoff passed its one-minute cap: %+v", backoff)
	}
	elapse()
	handlerErr, stewardErr = errors.New("handler boom"), errors.New("steward boom")
	if got, stderr := run(); got != "handler,steward" || backoff.delay != time.Minute || !backoff.ready(time.Now()) ||
		!strings.Contains(stderr, "[tt relay] handler rotation: handler boom\n") || !strings.Contains(stderr, "[tt relay] steward rotation: steward boom\n") {
		t.Fatalf("other errors: ticks=%q backoff=%+v stderr=%q", got, backoff, stderr)
	}
	handlerErr, stewardErr = nil, nil
	if got, stderr := run(); got != "handler,steward" || backoff.delay != 0 || !backoff.ready(time.Now()) || stderr != "" {
		t.Fatalf("success did not clear the backoff: ticks=%q backoff=%+v stderr=%q", got, backoff, stderr)
	}
	stewardErr = rateLimited
	if got, stderr := run(); got != "handler,steward" || backoff.delay != 6*time.Second || backoff.ready(time.Now()) || !strings.Contains(stderr, "[tt relay] steward rotation: ") {
		t.Fatalf("steward 429: ticks=%q backoff=%+v stderr=%q", got, backoff, stderr)
	}
	// A clean pass must not clear spacing that a 429 set while it ran.
	handlerErr, stewardErr = nil, nil
	rotation.start()
	rotation.collect(&backoff, true)
	if backoff.delay != 6*time.Second || backoff.ready(time.Now()) {
		t.Fatalf("a clean pass cleared a live backoff: %+v", backoff)
	}
}

func deployerNotice(seq int64, from string) api.Message {
	return api.Message{Seq: seq, From: api.Sender{AgentID: from, Node: "workspace", User: "owner"}, Text: "NOTICE: Release refused before publication",
		Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Release refused before publication", Body: api.EnvelopeBody{Text: "Release was refused before publication."}}}
}

// noticeHub is an isolated hub for the deployer notice wake: it serves each
// agent by its ID and records every call, so a test can prove which hub reads
// a binding caused and that the relay wrote nothing.
type noticeHub struct {
	t        *testing.T
	mu       sync.Mutex
	agents   map[string]api.Agent
	messages []api.Message
	calls    []string
	fail     map[string]int // agent ID -> status for its reads
	primary  string         // the project's primary database handler
}

func (h *noticeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	h.calls = append(h.calls, r.Method+" "+r.URL.Path)
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch {
	case r.Method != http.MethodGet:
		h.t.Errorf("the inbox wake must not write to the hub: %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	case strings.HasSuffix(r.URL.Path, "/pause"):
		_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
	case strings.HasSuffix(r.URL.Path, "/obligations"):
		_ = json.NewEncoder(w).Encode(api.ObligationList{})
	case strings.HasSuffix(r.URL.Path, "/messages"):
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		var page []api.Message
		for _, m := range h.messages {
			if m.Seq > after {
				page = append(page, m)
			}
		}
		_ = json.NewEncoder(w).Encode(api.MessageList{Messages: page})
	case r.URL.Path == "/v1/tasks" && h.fail["tasks"] != 0:
		http.Error(w, "refused", h.fail["tasks"])
	case r.URL.Path == "/v1/tasks":
		_ = json.NewEncoder(w).Encode(api.TaskList{Tasks: []api.Task{{ID: "tsk_00000000000000ff", PrimaryHandlerID: noticeHandlerB}, {ID: noticeTask, PrimaryHandlerID: h.primary}}})
	case strings.Contains(r.URL.Path, "/agents/") && h.fail[id] != 0:
		http.Error(w, "refused", h.fail[id])
	case strings.Contains(r.URL.Path, "/agents/"):
		a, ok := h.agents[id]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(a)
	default:
		h.t.Errorf("unexpected hub call %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

// reads counts the recorded reads of one agent, or of the task list for
// "tasks", and with reset clears the record.
func (h *noticeHub) reads(agent string, reset bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, call := range h.calls {
		if strings.HasSuffix(call, "/agents/"+agent) || (agent == "tasks" && call == "GET /v1/tasks") {
			n++
		}
	}
	if reset {
		h.calls = nil
	}
	return n
}

const (
	noticeTask     = "tsk_00000000000000d1"
	noticeDeployer = "agt_00000000000000d1"
	noticeHandler  = "agt_00000000000000d2"
	noticeHelper   = "agt_00000000000000d3"
	noticeWorker   = "agt_00000000000000d4"
	noticeSteward  = "agt_00000000000000d5"
	noticeHandlerB = "agt_00000000000000d6" // a live handler that is not the primary
	noticeRun      = "run_00000000000000d1"
)

func newNoticeHub(t *testing.T) (*noticeHub, *api.Client, func(agent string) runtimeBinding) {
	t.Helper()
	relayAuthorRoles.reset()
	relayPrimaryHandlers.reset()
	h := &noticeHub{t: t, agents: map[string]api.Agent{}, fail: map[string]int{}, primary: noticeHandler}
	for id, role := range map[string]string{noticeDeployer: api.AgentRoleDeployment, noticeHandler: api.AgentRoleDatabaseHandler, noticeHandlerB: api.AgentRoleDatabaseHandler,
		noticeHelper: api.AgentRoleOwnerHelper, noticeWorker: "", noticeSteward: api.AgentRoleBacklogSteward} {
		h.agents[id] = api.Agent{ID: id, RunID: noticeRun, Role: role, Runtime: "codex", Status: api.AgentDone, Online: true, Unread: 1}
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, _ := api.NewClient(srv.URL, time.Second)
	return h, c, func(agent string) runtimeBinding {
		b := runtimeBinding{Hub: srv.URL, Task: noticeTask, Agent: agent, Run: noticeRun, Thread: "00000000-0000-4000-8000-0000000000d1", Runtime: "codex", Codex: "/synthetic/codex"}
		if agent == noticeHelper {
			b.Role = api.AgentRoleOwnerHelper
		}
		return b
	}
}

// A release the deployer refuses, fails or holds is announced with no
// recipient. In a project that is not a swarm that reaches the database
// handler's and the owner helper's inbox, and it must also wake them
// (wi_3670e153328df185). This runs against a real hub store, so the inbox
// page and the unread count are the hub's own.
func TestRelayWakesHandlerAndHelperForDeployerNotice(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "cli-test", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Deployer notice", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, role string) api.Agent {
		t.Helper()
		a, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "host", Session: name, Runtime: "codex", Role: role}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	deployer, primary, other := add("deployer", api.AgentRoleDeployment), add("database", api.AgentRoleDatabaseHandler), add("database-item", api.AgentRoleDatabaseHandler)
	registered, err := st.RegisterOwnerHelper(ctx, task.ID, api.RegisterOwnerHelperRequest{Host: "host", Session: "owner", Runtime: "codex", RequestID: "notice-helper"}, by)
	if err != nil || registered.Agent == nil {
		t.Fatalf("register helper: %+v %v", registered, err)
	}
	helper := *registered.Agent
	for _, a := range []api.Agent{primary, other, helper} {
		if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: a.ID, RunID: a.RunID}, by); err != nil {
			t.Fatal(err)
		}
	}
	notice, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: deployer.ID, RunID: deployer.RunID, RequestID: "notice-refused",
		Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Release refused before publication", Body: api.EnvelopeBody{Text: "Release was refused before publication; nothing was published."}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	if notice.To != "" || notice.Broadcast || notice.From.AgentID != deployer.ID {
		t.Fatalf("fixture notice is not a recipient-less deployer notice: %+v", notice)
	}
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	poll := func(t *testing.T, a api.Agent, role string) ([]string, relayProgress) {
		t.Helper()
		if got, err := c.GetAgent(ctx, task.ID, a.ID); err != nil || got.Unread != 1 || !got.Online {
			t.Fatalf("the notice is not unread for live %s: %+v %v", a.Name, got, err)
		}
		b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: a.ID, Run: a.RunID, Thread: "00000000-0000-4000-8000-000000000001", Runtime: "codex", Role: role, Codex: "/synthetic/codex"}
		p := relayProgress{}
		var prompts []string
		queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
			prompts = append(prompts, prompt)
			return nil
		}
		now := time.Now()
		// The notice stays unread; a later poll must not wake for it again.
		for _, at := range []time.Time{now, now.Add(30 * time.Second)} {
			if err := relayOne(ctx, b, &p, c, at, queue); err != nil {
				t.Fatal(err)
			}
		}
		return prompts, p
	}
	woken := func(t *testing.T, a api.Agent, role string) {
		t.Helper()
		prompts, p := poll(t, a, role)
		if len(prompts) != 1 || p.Through != notice.Seq || !strings.Contains(prompts[0], fmt.Sprintf("through message #%d", notice.Seq)) {
			t.Fatalf("%s was not woken once for deployer notice #%d: prompts=%q progress=%+v", a.Name, notice.Seq, prompts, p)
		}
	}
	asleep := func(t *testing.T, a api.Agent) {
		t.Helper()
		if prompts, p := poll(t, a, ""); len(prompts) != 0 || p.Through != notice.Seq {
			t.Fatalf("%s was woken for deployer notice #%d: prompts=%q progress=%+v", a.Name, notice.Seq, prompts, p)
		}
	}
	// With no primary handler set, no handler is woken; the owner helper is.
	t.Run("no primary handler", func(t *testing.T) {
		relayAuthorRoles.reset()
		relayPrimaryHandlers.reset()
		asleep(t, primary)
		asleep(t, other)
		woken(t, helper, api.AgentRoleOwnerHelper)
	})
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET primary_handler_id=? WHERE id=?`, primary.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	// Of two live handlers only the project's primary is woken.
	t.Run("primary handler", func(t *testing.T) {
		relayAuthorRoles.reset()
		relayPrimaryHandlers.reset()
		woken(t, primary, "")
		asleep(t, other)
		woken(t, helper, api.AgentRoleOwnerHelper)
	})
}

// A project holds one live database handler per running item. Only the
// primary is woken; another handler reads no author's role, and the task list
// is read once for both.
func TestRelayDeployerNoticeWakesOnlyPrimaryHandler(t *testing.T) {
	h, c, binding := newNoticeHub(t)
	h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	ctx := context.Background()
	now := time.Now()
	woken := map[string]int{}
	queue := func(_ context.Context, b runtimeBinding, _ string) error {
		woken[b.Agent]++
		return nil
	}
	progress := map[string]*relayProgress{noticeHandlerB: {}, noticeHandler: {}}
	for i := 0; i < 3; i++ {
		for _, agent := range []string{noticeHandlerB, noticeHandler} {
			if err := relayOne(ctx, binding(agent), progress[agent], c, now.Add(time.Duration(i)*20*time.Second), queue); err != nil {
				t.Fatal(err)
			}
		}
	}
	if woken[noticeHandler] != 1 || len(woken) != 1 || progress[noticeHandlerB].Through != 31 {
		t.Fatalf("woken=%v other=%+v", woken, progress[noticeHandlerB])
	}
	if tasks, author := h.reads("tasks", false), h.reads(noticeDeployer, false); tasks != 1 || author != 1 {
		t.Fatalf("task list read %d times and the deployer %d times for two handlers", tasks, author)
	}
	// The other handler alone: the task list, and never the author.
	h2, c2, binding2 := newNoticeHub(t)
	h2.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	p := relayProgress{}
	never := func(context.Context, runtimeBinding, string) error {
		t.Fatal("a handler that is not the primary was woken")
		return nil
	}
	for i := 0; i < 3; i++ {
		if err := relayOne(ctx, binding2(noticeHandlerB), &p, c2, now.Add(time.Duration(i)*time.Minute), never); err != nil {
			t.Fatal(err)
		}
	}
	if tasks, author := h2.reads("tasks", false), h2.reads(noticeDeployer, false); tasks != 1 || author != 0 || p.Through != 31 {
		t.Fatalf("a handler that is not the primary read the task list %d times and the deployer %d times: %+v", tasks, author, p)
	}
	// A committed rotation moves the wake to the new primary within a minute.
	h2.mu.Lock()
	h2.primary = noticeHandlerB
	h2.messages = append(h2.messages, deployerNotice(32, noticeDeployer))
	h2.mu.Unlock()
	if err := relayOne(ctx, binding2(noticeHandlerB), &p, c2, now.Add(5*time.Minute), queue); err != nil || woken[noticeHandlerB] != 1 || p.Through != 32 {
		t.Fatalf("the new primary was not woken: %v %v %+v", err, woken, p)
	}
}

// With no primary handler set, no handler is woken and no author is read; the
// owner helper's wake does not depend on it.
func TestRelayDeployerNoticeWithoutPrimaryHandler(t *testing.T) {
	h, c, binding := newNoticeHub(t)
	h.primary = ""
	h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	woken := map[string]int{}
	queue := func(_ context.Context, b runtimeBinding, _ string) error {
		woken[b.Agent]++
		return nil
	}
	now := time.Now()
	for _, agent := range []string{noticeHandler, noticeHandlerB} {
		p := relayProgress{}
		if err := relayOne(context.Background(), binding(agent), &p, c, now, queue); err != nil || p.Through != 31 {
			t.Fatalf("%v %+v", err, p)
		}
	}
	if len(woken) != 0 || h.reads(noticeDeployer, false) != 0 || h.reads("tasks", false) != 1 {
		t.Fatalf("no primary: woken=%v deployer reads=%d", woken, h.reads(noticeDeployer, false))
	}
	if err := relayOne(context.Background(), binding(noticeHelper), &relayProgress{}, c, now, queue); err != nil || woken[noticeHelper] != 1 || len(woken) != 1 {
		t.Fatalf("the owner helper was not woken: %v %v", err, woken)
	}
	if h.reads("tasks", false) != 1 {
		t.Fatal("the owner helper's wake read the task list")
	}
}

func TestRelayDeployerNoticeWakesOncePerRecipient(t *testing.T) {
	for _, agent := range []string{noticeHandler, noticeHelper} {
		for _, runtime := range []string{"codex", "claude"} {
			t.Run(agent+"-"+runtime, func(t *testing.T) {
				h, c, binding := newNoticeHub(t)
				h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
				b := binding(agent)
				if runtime == "claude" {
					b.Runtime, b.Codex, b.Session = "claude", "", "fixture"
				}
				ctx := context.Background()
				p := relayProgress{}
				var prompts []string
				queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
					prompts = append(prompts, prompt)
					return nil
				}
				failed := func(context.Context, runtimeBinding, string) error { return errors.New("Codex queue failed: offline") }
				now := time.Now()
				// A failed queue is not a delivered wake: the cursor stays and the
				// retry is the one wake.
				if err := relayOne(ctx, b, &p, c, now, failed); err == nil || p.Through != 0 {
					t.Fatalf("failed queue advanced the cursor: %v %+v", err, p)
				}
				if err := relayOne(ctx, b, &p, c, now.Add(time.Second), queue); err != nil || len(prompts) != 0 {
					t.Fatalf("retry ignored the 15-second spacing: %v %q", err, prompts)
				}
				if err := relayOne(ctx, b, &p, c, now.Add(time.Minute), queue); err != nil || len(prompts) != 1 || p.Through != 31 {
					t.Fatalf("retry did not wake once: %v %q %+v", err, prompts, p)
				}
				want := "through message #31"
				if runtime == "claude" {
					want = "Tailterm messages #31."
				}
				inbox := "tt inbox --unread --mark-read"
				if agent == noticeHelper {
					inbox = "tt helper inbox --task " + noticeTask
				}
				if !strings.Contains(prompts[0], want) || !strings.Contains(prompts[0], inbox) {
					t.Fatalf("wake prompt %q", prompts[0])
				}
				// Repeated polls, a persisted and reloaded progress record, and the
				// same page read again all leave it at one wake.
				raw, _ := json.Marshal(p)
				p = relayProgress{}
				_ = json.Unmarshal(raw, &p)
				for i := 2; i < 6; i++ {
					if err := relayOne(ctx, b, &p, c, now.Add(time.Duration(i)*time.Minute), queue); err != nil {
						t.Fatal(err)
					}
				}
				if len(prompts) != 1 {
					t.Fatalf("one notice woke %d times: %q", len(prompts), prompts)
				}
				// The author's role is read once for the relay process, however
				// many polls and notices follow.
				h.mu.Lock()
				h.messages = append(h.messages, deployerNotice(32, noticeDeployer))
				h.mu.Unlock()
				if err := relayOne(ctx, b, &p, c, now.Add(10*time.Minute), queue); err != nil || len(prompts) != 2 || p.Through != 32 {
					t.Fatalf("a second notice did not wake once: %v %q %+v", err, prompts, p)
				}
				if n := h.reads(noticeDeployer, false); n != 1 {
					t.Fatalf("the deployer's role was read %d times", n)
				}
			})
		}
	}
}

// A binding that is not live gets no wake and causes no hub request beyond
// the ones the relay already made for it.
func TestRelayDeployerNoticeNeedsLiveBinding(t *testing.T) {
	for name, change := range map[string]func(*api.Agent){
		"superseded run": func(a *api.Agent) { a.RunID = "run_00000000000000ff" },
		"offline":        func(a *api.Agent) { a.Online = false },
		"closed":         func(a *api.Agent) { a.Status = api.AgentClosed },
		"exited":         func(a *api.Agent) { a.Status = api.AgentExited },
		"retired":        func(a *api.Agent) { a.Status = api.AgentRetired },
	} {
		for _, agent := range []string{noticeHandler, noticeHelper} {
			t.Run(name+"-"+agent, func(t *testing.T) {
				h, c, binding := newNoticeHub(t)
				h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
				a := h.agents[agent]
				change(&a)
				h.agents[agent] = a
				p := relayProgress{}
				queue := func(context.Context, runtimeBinding, string) error {
					t.Fatal("a binding that is not live was woken")
					return nil
				}
				if err := relayOne(context.Background(), binding(agent), &p, c, time.Now(), queue); err != nil {
					t.Fatal(err)
				}
				h.mu.Lock()
				calls := strings.Join(h.calls, ", ")
				h.mu.Unlock()
				if want := "GET /v1/tasks/" + noticeTask + "/pause, GET /v1/tasks/" + noticeTask + "/agents/" + agent; calls != want {
					t.Fatalf("hub requests for a binding that is not live: %s", calls)
				}
			})
		}
	}
	// An agent with no binding is never passed to relayOne; the bindings of
	// other agents must not cause a read of it or a wake for it.
	h, c, binding := newNoticeHub(t)
	h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	woken := map[string]int{}
	queue := func(_ context.Context, b runtimeBinding, _ string) error {
		woken[b.Agent]++
		return nil
	}
	if err := relayOne(context.Background(), binding(noticeHandler), &relayProgress{}, c, time.Now(), queue); err != nil {
		t.Fatal(err)
	}
	if woken[noticeHandler] != 1 || len(woken) != 1 || h.reads(noticeHelper, false) != 0 {
		t.Fatalf("the unbound owner helper was read or woken: %v", woken)
	}
}

func TestRelayDeployerNoticeLeavesOtherWakesUnchanged(t *testing.T) {
	result := deployerNotice(31, noticeDeployer)
	result.Envelope = &api.Envelope{Kind: api.EnvelopeKindResult, Subject: "Release published"}
	freeText := api.Message{Seq: 31, From: api.Sender{AgentID: noticeDeployer}, Text: "deployer free text"}
	broker := deployerNotice(31, noticeDeployer)
	broker.From = api.Sender{Node: api.BrokerNode, User: "broker"}
	directedElsewhere := deployerNotice(31, noticeDeployer)
	directedElsewhere.To = noticeWorker
	directed := deployerNotice(31, noticeDeployer)
	directed.To = noticeWorker
	broadcast := deployerNotice(31, noticeWorker)
	broadcast.Broadcast = true
	for _, tc := range []struct {
		name, agent string
		m           api.Message
		wake        bool
		authorReads int // reads of the message's author
	}{
		{"deployer notice wakes the handler", noticeHandler, deployerNotice(31, noticeDeployer), true, 1},
		{"deployer notice wakes the helper", noticeHelper, deployerNotice(31, noticeDeployer), true, 1},
		{"worker notice does not wake the handler", noticeHandler, deployerNotice(31, noticeWorker), false, 1},
		{"steward notice does not wake the helper", noticeHelper, deployerNotice(31, noticeSteward), false, 1},
		{"handler notice does not wake the helper", noticeHelper, deployerNotice(31, noticeHandler), false, 1},
		{"unknown author does not wake the handler", noticeHandler, deployerNotice(31, "agt_00000000000000ee"), false, 1},
		{"deployer result does not wake the handler", noticeHandler, result, false, 0},
		{"deployer free text does not wake the helper", noticeHelper, freeText, false, 0},
		{"broker notice does not wake the handler", noticeHandler, broker, false, 0},
		{"notice directed elsewhere does not wake the handler", noticeHandler, directedElsewhere, false, 0},
		{"deployer notice does not wake another handler", noticeHandlerB, deployerNotice(31, noticeDeployer), false, 0},
		{"deployer notice does not wake a worker", noticeWorker, deployerNotice(31, noticeDeployer), false, 0},
		{"deployer notice does not wake the steward", noticeSteward, deployerNotice(31, noticeDeployer), false, 0},
		{"deployer notice does not wake the deployer", noticeDeployer, deployerNotice(31, noticeDeployer), false, 0},
		{"directed still wakes a worker", noticeWorker, directed, true, 0},
		{"broadcast still wakes the steward", noticeSteward, broadcast, true, 0},
		{"authorless still wakes a worker", noticeWorker, api.Message{Seq: 31, From: api.Sender{Node: "workspace", User: "owner"}, Text: "owner note"}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c, binding := newNoticeHub(t)
			h.messages = []api.Message{tc.m}
			p := relayProgress{}
			wakes := 0
			queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
			now := time.Now()
			for i := 0; i < 3; i++ {
				if err := relayOne(context.Background(), binding(tc.agent), &p, c, now.Add(time.Duration(i)*time.Minute), queue); err != nil {
					t.Fatal(err)
				}
			}
			if want := map[bool]int{true: 1}[tc.wake]; wakes != want || p.Through != 31 {
				t.Fatalf("wakes=%d want %d progress=%+v", wakes, want, p)
			}
			author := tc.m.From.AgentID
			reads := h.reads(author, false)
			if author == tc.agent {
				reads = 0 // its own reads, not an author lookup
			}
			if tc.authorReads == 0 && author != "" && author != tc.agent && reads != 0 || tc.authorReads == 1 && reads != 1 {
				t.Fatalf("author %s was read %d times, want %d", author, reads, tc.authorReads)
			}
			// One rule for the wake, its diagnostics and the Claude prompt.
			authors := map[string]bool{}
			if tc.wake && tc.authorReads == 1 {
				authors[author] = true
			}
			_, eligible := wakeThroughFor(h.messages, tc.agent, authors)
			seqs := wakeSeqsFor(h.messages, tc.agent, authors)
			prompt := claudeWakePromptFor(h.messages, tc.agent, authors)
			if eligible != tc.wake || (len(seqs) == 1) != tc.wake || strings.Contains(prompt, "#31") != tc.wake {
				t.Fatalf("rule disagrees: eligible=%v seqs=%v prompt=%q", eligible, seqs, prompt)
			}
			// Without the deployer set, the rule is what it was before.
			_, before := wakeThrough(h.messages, tc.agent)
			if want := tc.wake && tc.authorReads == 0; before != want {
				t.Fatalf("existing rule changed: %v want %v", before, want)
			}
		})
	}
}

// A failed role read holds the page instead of passing the notice over, and a
// later poll delivers it.
func TestRelayDeployerNoticeRoleReadFailureRetries(t *testing.T) {
	h, c, binding := newNoticeHub(t)
	h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	h.fail[noticeDeployer] = http.StatusInternalServerError
	p := relayProgress{}
	wakes := 0
	queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
	now := time.Now()
	if err := relayOne(context.Background(), binding(noticeHandler), &p, c, now, queue); err == nil || wakes != 0 || p.Through != 0 {
		t.Fatalf("failed role read: err=%v wakes=%d progress=%+v", err, wakes, p)
	}
	h.mu.Lock()
	delete(h.fail, noticeDeployer)
	h.mu.Unlock()
	if err := relayOne(context.Background(), binding(noticeHandler), &p, c, now.Add(time.Minute), queue); err != nil || wakes != 1 || p.Through != 31 {
		t.Fatalf("recovered role read: err=%v wakes=%d progress=%+v", err, wakes, p)
	}
	// The same holds for the read of the project's primary handler.
	h, c, binding = newNoticeHub(t)
	h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	h.fail["tasks"] = http.StatusInternalServerError
	p, wakes = relayProgress{}, 0
	if err := relayOne(context.Background(), binding(noticeHandler), &p, c, now, queue); err == nil || wakes != 0 || p.Through != 0 {
		t.Fatalf("failed task list read: err=%v wakes=%d progress=%+v", err, wakes, p)
	}
	h.mu.Lock()
	delete(h.fail, "tasks")
	h.mu.Unlock()
	if err := relayOne(context.Background(), binding(noticeHandler), &p, c, now.Add(time.Minute), queue); err != nil || wakes != 1 || p.Through != 31 {
		t.Fatalf("recovered task list read: err=%v wakes=%d progress=%+v", err, wakes, p)
	}
}

// taggedNotice is a recipient-less deployer notice with the runner's wake ref
// (wi_991125f66a787459); an empty wake is the untagged notice of an older
// runner.
func taggedNotice(seq int64, from, subject, wake string) api.Message {
	m := deployerNotice(seq, from)
	m.Text = "NOTICE: " + subject
	m.Envelope.Subject = subject
	m.Envelope.Refs = map[string]string{"release-job": "rel_00000000000000d1"}
	if wake != "" {
		m.Envelope.Refs["wake"] = wake
	}
	return m
}

// noticeRecipients are the two agents a deployer notice can wake.
var noticeRecipients = []string{noticeHandler, noticeHelper}

// A notice the runner tags routine wakes neither the primary handler nor the
// owner helper. The cursor moves past it, and it causes no read of its
// author's role or of the task list.
func TestRelayDeployerNoticeRoutineWakesNobody(t *testing.T) {
	for _, agent := range noticeRecipients {
		for _, runtime := range []string{"codex", "claude"} {
			t.Run(agent+"-"+runtime, func(t *testing.T) {
				h, c, binding := newNoticeHub(t)
				h.messages = []api.Message{taggedNotice(31, noticeDeployer, "A release job is waiting behind a held project fence", "routine")}
				b := binding(agent)
				if runtime == "claude" {
					b.Runtime, b.Codex, b.Session = "claude", "", "fixture"
				}
				never := func(context.Context, runtimeBinding, string) error {
					t.Fatal("a routine deployer notice queued a wake")
					return nil
				}
				p := relayProgress{}
				now := time.Now()
				if err := relayOne(context.Background(), b, &p, c, now, never); err != nil {
					t.Fatal(err)
				}
				h.mu.Lock()
				calls := strings.Join(h.calls, ", ")
				h.mu.Unlock()
				base := "GET /v1/tasks/" + noticeTask
				if want := base + "/pause, " + base + "/agents/" + agent + ", " + base + "/messages"; calls != want {
					t.Fatalf("hub requests for a routine notice: %s", calls)
				}
				if p.Through != 31 || p.Wakes != 0 || !p.LastAttempt.IsZero() || p.Skip != nil {
					t.Fatalf("progress after a routine notice: %+v", p)
				}
				// Later passes, a reloaded progress record and a second routine
				// notice change nothing.
				raw, _ := json.Marshal(p)
				p = relayProgress{}
				_ = json.Unmarshal(raw, &p)
				h.mu.Lock()
				h.messages = append(h.messages, taggedNotice(32, noticeDeployer, "A release job is waiting for the verification host", "routine"))
				h.mu.Unlock()
				for i := 1; i < 4; i++ {
					if err := relayOne(context.Background(), b, &p, c, now.Add(time.Duration(i)*time.Minute), never); err != nil {
						t.Fatal(err)
					}
				}
				if tasks, author := h.reads("tasks", false), h.reads(noticeDeployer, false); tasks != 0 || author != 0 || p.Through != 32 {
					t.Fatalf("routine notices read the task list %d times and the deployer %d times: %+v", tasks, author, p)
				}
			})
		}
	}
}

// Only the exact routine tag is quiet. An attention notice, an untagged one
// (an older runner) and one with any other value wake both recipients once.
func TestRelayDeployerNoticeAttentionUntaggedAndUnknownWake(t *testing.T) {
	for _, wake := range []string{"attention", "", "urgent", "Routine", "routine ", "ROUTINE", "routine,attention"} {
		for _, agent := range noticeRecipients {
			t.Run(fmt.Sprintf("%q-%s", wake, agent), func(t *testing.T) {
				h, c, binding := newNoticeHub(t)
				m := taggedNotice(31, noticeDeployer, "Release failed and requires recovery", wake)
				if _, tagged := m.Envelope.Refs["wake"]; tagged != (wake != "") {
					t.Fatalf("fixture refs: %v", m.Envelope.Refs)
				}
				h.messages = []api.Message{m}
				p := relayProgress{}
				wakes := 0
				queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
				now := time.Now()
				for i := 0; i < 3; i++ {
					if err := relayOne(context.Background(), binding(agent), &p, c, now.Add(time.Duration(i)*time.Minute), queue); err != nil {
						t.Fatal(err)
					}
				}
				if wakes != 1 || p.Through != 31 || h.reads(noticeDeployer, false) != 1 {
					t.Fatalf("wakes=%d author reads=%d progress=%+v", wakes, h.reads(noticeDeployer, false), p)
				}
			})
		}
	}
	// A notice with no refs at all, and one whose other refs say routine.
	bare := deployerNotice(31, noticeDeployer)
	other := taggedNotice(31, noticeDeployer, "Release refused before publication", "")
	other.Envelope.Refs["release-job"] = "routine"
	other.Envelope.Refs["Wake"] = "routine"
	for name, m := range map[string]api.Message{"no refs": bare, "other refs": other} {
		if !wakingNotice(m) || !wakeEligible(m, noticeHelper, map[string]bool{noticeDeployer: true}) {
			t.Fatalf("%s: the notice does not wake", name)
		}
	}
}

// The wake ref is kept by the real store and served by the hub API, and the
// relay reads it there: a routine notice wakes nobody, an attention notice
// posted after it wakes the primary handler and the owner helper once.
func TestRelayDeployerNoticeWakeRefThroughStore(t *testing.T) {
	relayAuthorRoles.reset()
	relayPrimaryHandlers.reset()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "cli-test", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Deployer wake class", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, role string) api.Agent {
		t.Helper()
		a, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "host", Session: name, Runtime: "codex", Role: role}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	deployer, primary := add("deployer", api.AgentRoleDeployment), add("database", api.AgentRoleDatabaseHandler)
	registered, err := st.RegisterOwnerHelper(ctx, task.ID, api.RegisterOwnerHelperRequest{Host: "host", Session: "owner", Runtime: "codex", RequestID: "wake-helper"}, by)
	if err != nil || registered.Agent == nil {
		t.Fatalf("register helper: %+v %v", registered, err)
	}
	helper := *registered.Agent
	for _, a := range []api.Agent{primary, helper} {
		if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: a.ID, RunID: a.RunID}, by); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET primary_handler_id=? WHERE id=?`, primary.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	post := func(requestID, subject, wake string) api.Message {
		t.Helper()
		m, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: deployer.ID, RunID: deployer.RunID, RequestID: requestID,
			Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: map[string]string{"release-job": "rel_00000000000000d1", "wake": wake}, Body: api.EnvelopeBody{Text: subject + "."}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		if m.To != "" || m.Broadcast {
			t.Fatalf("fixture notice has a recipient: %+v", m)
		}
		return m
	}
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	served := func(m api.Message, agent, wake string) {
		t.Helper()
		page, err := c.ListMessages(ctx, task.ID, m.Seq-1, agent, 200)
		if err != nil || len(page) != 1 || page[0].Seq != m.Seq || page[0].Envelope == nil || page[0].Envelope.Refs["wake"] != wake {
			t.Fatalf("the hub did not serve notice #%d with wake=%s: %+v %v", m.Seq, wake, page, err)
		}
	}
	type recipient struct {
		b       runtimeBinding
		p       relayProgress
		prompts []string
	}
	recipients := map[string]*recipient{}
	for _, a := range []api.Agent{primary, helper} {
		recipients[a.Name] = &recipient{b: runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: a.ID, Run: a.RunID, Thread: "00000000-0000-4000-8000-000000000001", Runtime: "codex", Role: a.Role, Codex: "/synthetic/codex"}}
	}
	recipients[primary.Name].b.Role = ""
	now := time.Now()
	pass := func(at time.Time) {
		t.Helper()
		for _, r := range recipients {
			queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
				r.prompts = append(r.prompts, prompt)
				return nil
			}
			if err := relayOne(ctx, r.b, &r.p, c, at, queue); err != nil {
				t.Fatal(err)
			}
		}
	}
	routine := post("wake-routine", "A release job is waiting behind a held project fence", "routine")
	for name, r := range recipients {
		served(routine, r.b.Agent, "routine")
		if a, err := c.GetAgent(ctx, task.ID, r.b.Agent); err != nil || a.Unread != 1 || !a.Online {
			t.Fatalf("the routine notice is not unread for live %s: %+v %v", name, a, err)
		}
	}
	pass(now)
	pass(now.Add(time.Minute))
	for name, r := range recipients {
		if len(r.prompts) != 0 || r.p.Through != routine.Seq {
			t.Fatalf("%s after the routine notice: prompts=%q progress=%+v", name, r.prompts, r.p)
		}
	}
	attention := post("wake-attention", "Release failed and requires recovery", "attention")
	for _, r := range recipients {
		served(attention, r.b.Agent, "attention")
	}
	pass(now.Add(2 * time.Minute))
	pass(now.Add(3 * time.Minute))
	for name, r := range recipients {
		if len(r.prompts) != 1 || r.p.Through != attention.Seq || !strings.Contains(r.prompts[0], fmt.Sprintf("through message #%d", attention.Seq)) {
			t.Fatalf("%s was not woken once for attention notice #%d: prompts=%q progress=%+v", name, attention.Seq, r.prompts, r.p)
		}
	}
}

// An attention notice wakes each recipient exactly once across repeated
// passes and a reloaded progress record, and on a page that also holds
// routine notices the one wake names the attention notice only.
func TestRelayDeployerNoticeAttentionWakesOnceAndMixedPage(t *testing.T) {
	for _, agent := range noticeRecipients {
		t.Run("once-"+agent, func(t *testing.T) {
			h, c, binding := newNoticeHub(t)
			h.messages = []api.Message{taggedNotice(31, noticeDeployer, "Release refused before publication", "attention")}
			p := relayProgress{}
			wakes := 0
			queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
			now := time.Now()
			for i := 0; i < 8; i++ {
				if i == 3 {
					raw, _ := json.Marshal(p)
					p = relayProgress{}
					if err := json.Unmarshal(raw, &p); err != nil {
						t.Fatal(err)
					}
				}
				if err := relayOne(context.Background(), binding(agent), &p, c, now.Add(time.Duration(i)*time.Minute), queue); err != nil {
					t.Fatal(err)
				}
				if wakes != 1 || p.Through != 31 {
					t.Fatalf("pass %d: wakes=%d progress=%+v", i, wakes, p)
				}
			}
		})
		t.Run("mixed-"+agent, func(t *testing.T) {
			h, c, binding := newNoticeHub(t)
			h.messages = []api.Message{
				taggedNotice(31, noticeDeployer, "A release job is waiting behind a held project fence", "routine"),
				taggedNotice(32, noticeDeployer, "Release failed and requires recovery", "attention"),
				taggedNotice(33, noticeDeployer, "A deployer CLI call failed", "routine"),
			}
			b := binding(agent)
			b.Runtime, b.Codex, b.Session = "claude", "", "fixture"
			p := relayProgress{}
			var prompts []string
			queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
				prompts = append(prompts, prompt)
				return nil
			}
			now := time.Now()
			for i := 0; i < 3; i++ {
				if err := relayOne(context.Background(), b, &p, c, now.Add(time.Duration(i)*time.Minute), queue); err != nil {
					t.Fatal(err)
				}
			}
			if len(prompts) != 1 || p.Through != 33 || !strings.HasPrefix(prompts[0], "Tailterm messages #32. ") || strings.Contains(prompts[0], "#31") || strings.Contains(prompts[0], "#33") {
				t.Fatalf("mixed page: prompts=%q progress=%+v", prompts, p)
			}
			if h.reads(noticeDeployer, false) != 1 {
				t.Fatalf("the deployer's role was read %d times", h.reads(noticeDeployer, false))
			}
		})
	}
}

// The routine tag is read only on a deployer's recipient-less notice. On a
// message directed to the agent, a broadcast or an owner post it stops no
// wake, and an attention notice still wakes no handler but the primary.
func TestRelayDeployerNoticeRoutineTagStopsNoOtherWake(t *testing.T) {
	directed := func(to string) api.Message {
		m := taggedNotice(31, noticeDeployer, "A release job is skipped because the hub refused its claim", "routine")
		m.To = to
		return m
	}
	broadcast := taggedNotice(31, noticeDeployer, "A release job is waiting behind a held project fence", "routine")
	broadcast.Broadcast = true
	owner := taggedNotice(31, "", "A release job is waiting behind a held project fence", "routine")
	if owner.From.AgentID != "" || owner.Envelope.Refs["wake"] != "routine" {
		t.Fatalf("fixture owner post: %+v", owner)
	}
	for _, tc := range []struct {
		name, agent string
		m           api.Message
		wake        bool
	}{
		{"directed to the handler", noticeHandler, directed(noticeHandler), true},
		{"directed to the helper", noticeHelper, directed(noticeHelper), true},
		{"directed to a worker", noticeWorker, directed(noticeWorker), true},
		{"broadcast to the handler", noticeHandler, broadcast, true},
		{"broadcast to the helper", noticeHelper, broadcast, true},
		{"broadcast to a worker", noticeWorker, broadcast, true},
		{"owner post to the handler", noticeHandler, owner, true},
		{"owner post to the helper", noticeHelper, owner, true},
		{"owner post to a worker", noticeWorker, owner, true},
		{"routine worker notice to the helper", noticeHelper, taggedNotice(31, noticeWorker, "Status", "routine"), false},
		{"attention worker notice to the helper", noticeHelper, taggedNotice(31, noticeWorker, "Status", "attention"), false},
		{"attention notice to another handler", noticeHandlerB, taggedNotice(31, noticeDeployer, "Release failed and requires recovery", "attention"), false},
		{"attention notice to a worker", noticeWorker, taggedNotice(31, noticeDeployer, "Release failed and requires recovery", "attention"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c, binding := newNoticeHub(t)
			h.messages = []api.Message{tc.m}
			p := relayProgress{}
			wakes := 0
			queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
			now := time.Now()
			for i := 0; i < 3; i++ {
				if err := relayOne(context.Background(), binding(tc.agent), &p, c, now.Add(time.Duration(i)*time.Minute), queue); err != nil {
					t.Fatal(err)
				}
			}
			if want := map[bool]int{true: 1}[tc.wake]; wakes != want || p.Through != 31 {
				t.Fatalf("wakes=%d want %d progress=%+v", wakes, want, p)
			}
			// A deployer's role is never read for a routine notice, nor by a
			// handler that is not the primary.
			if tc.m.From.AgentID == noticeDeployer && h.reads(noticeDeployer, false) != 0 {
				t.Fatalf("the deployer's role was read %d times", h.reads(noticeDeployer, false))
			}
		})
	}
}

// One clean release as the deployer announces it: three fence waits for
// normal handler steps, a wait for the verification host, and the restart
// onto the released scripts with its "now runs" report. Tagged as the runner
// now tags them (tests/release-runner.test.js wake a1), none wakes the primary
// handler or the owner helper. The same notices untagged, as before, woke each
// of them for every one.
func TestRelayDeployerNoticeCleanReleaseWakesNobody(t *testing.T) {
	subjects := []string{
		"A release job is waiting behind a held project fence",
		"A release job is waiting for the verification host",
		"A release job is waiting behind a held project fence",
		"A release job is waiting behind a held project fence",
		"Deployer is restarting itself onto the published scripts",
		"Deployer now runs the published scripts",
	}
	replay := func(t *testing.T, wake string) map[string]int {
		t.Helper()
		h, c, binding := newNoticeHub(t)
		woken := map[string]int{}
		queue := func(_ context.Context, b runtimeBinding, _ string) error {
			woken[b.Agent]++
			return nil
		}
		progress := map[string]*relayProgress{noticeHandler: {}, noticeHelper: {}}
		now := time.Now()
		// Each notice arrives, and the relay passes over both bindings before
		// the next one does.
		for i, subject := range subjects {
			h.mu.Lock()
			h.messages = append(h.messages, taggedNotice(int64(31+i), noticeDeployer, subject, wake))
			h.mu.Unlock()
			for _, agent := range noticeRecipients {
				if err := relayOne(context.Background(), binding(agent), progress[agent], c, now.Add(time.Duration(i)*time.Minute), queue); err != nil {
					t.Fatal(err)
				}
			}
		}
		for agent, p := range progress {
			if p.Through != int64(30+len(subjects)) {
				t.Fatalf("%s did not pass the last notice: %+v", agent, p)
			}
		}
		if wake == "routine" && (h.reads(noticeDeployer, false) != 0 || h.reads("tasks", false) != 0) {
			t.Fatalf("a clean release read the deployer %d times and the task list %d times", h.reads(noticeDeployer, false), h.reads("tasks", false))
		}
		return woken
	}
	if woken := replay(t, "routine"); len(woken) != 0 {
		t.Fatalf("a clean release woke %v", woken)
	}
	before := replay(t, "")
	if before[noticeHandler] != len(subjects) || before[noticeHelper] != len(subjects) || len(before) != 2 {
		t.Fatalf("the same release untagged woke %v, want %d each", before, len(subjects))
	}
}
