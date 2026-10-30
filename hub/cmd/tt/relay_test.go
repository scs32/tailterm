package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
				case strings.HasSuffix(r.URL.Path, "/usage/holds"), r.URL.Path == "/v1/provider-usage":
					http.NotFound(w, r) // Nor has it the token budget routes.
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
		case strings.HasSuffix(r.URL.Path, "/usage/holds"), r.URL.Path == "/v1/provider-usage":
			http.NotFound(w, r) // Nor has it the token budget routes.
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
	// The token budget hold of the agent's team: heldRun is the exact run the
	// hub lists as held (empty: none), noHolds a hub without the holds route
	// and holdsStatus a failing holds read.
	heldRun     string
	noHolds     bool
	holdsStatus int
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
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/usage/holds"):
		switch {
		case h.noHolds:
			http.NotFound(w, r)
		case h.holdsStatus != 0:
			http.Error(w, `{"error":"synthetic"}`, h.holdsStatus)
		default:
			q := r.URL.Query()
			_ = json.NewEncoder(w).Encode(api.UsageHolds{Held: h.heldRun != "" && q.Get("agent") == h.agent.ID && q.Get("run") == h.heldRun, Holds: []api.BudgetHold{}})
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
		job := api.WakeJob{}
		// Like the hub, lease nothing for a held run of an agent with no role.
		if held := h.heldRun != "" && h.heldRun == h.agent.RunID && h.agent.Role == "" && !h.noHolds; len(h.jobs) > 0 && !held {
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

// A Codex wake whose codex queue printed its confirmation and then failed,
// killed at the pass deadline or exiting non-zero, is queued: the broker job is
// reported accepted on a fresh context, the inbox cursor advances, and neither
// path queues it again. Without the confirmation the wake still fails.
func TestRelayCodexQueueConfirmedBeforeErrorCountsQueued(t *testing.T) {
	for _, tc := range []struct {
		name, script, exit string
		deadline, queued   bool
	}{
		{"confirmed-then-killed", "printf 'Queued message m1 for thread t1.\\n'\nexec sleep 30\n", "signal: killed", true, true},
		{"confirmed-then-exit-1", "printf 'Queued message m1 for thread t1.\\n'\nexit 1\n", "exit status 1", false, true},
		{"silent-killed", "exec sleep 30\n", "signal: killed", true, false},
		{"silent-exit-1", "exit 1\n", "exit status 1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			fake := filepath.Join(dir, "fake-codex")
			// exec, so a deadline kill reaches the process holding the output pipe.
			if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'queued\\n' >> '"+calls+"'\n"+tc.script), 0700); err != nil {
				t.Fatal(err)
			}
			queueCalls := func() int {
				data, _ := os.ReadFile(calls)
				return strings.Count(string(data), "queued\n")
			}
			pass := func(run func(context.Context) error) string {
				ctx, cancel := context.WithCancel(context.Background())
				if tc.deadline {
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				}
				defer cancel()
				out, err := captureRelayOutput(t, true, func() error { return run(ctx) })
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			checkLog := func(out string) {
				t.Helper()
				want := 0
				if tc.queued {
					want = 1
				}
				if got := strings.Count(out, "[tt relay] agt_0123456789abcdef Codex queued"); got != want || (tc.queued && !strings.Contains(out, tc.exit)) {
					t.Fatalf("relay log has %d confirmed-failure lines, want %d naming %q:\n%s", got, want, tc.exit, out)
				}
			}
			agent := api.Agent{ID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", Runtime: "codex", Status: api.AgentDone, Online: true, Unread: 1}
			b := runtimeBinding{Task: "tsk_0123456789abcdef", Agent: agent.ID, Run: agent.RunID, Thread: "00000000-0000-4000-8000-000000000001", Runtime: "codex", Codex: fake}

			// Broker path. The fixture re-serves an unreported job, as the hub
			// does once its lease expires.
			job := api.WakeJob{ID: "wake_0123456789abcdef", LeaseToken: "lease", MessageSeq: 42, AgentID: agent.ID, RunID: agent.RunID, Prompt: "Tailterm broker: #42 request. Run tt obligations."}
			h := &needsInputHub{t: t, agent: agent, jobs: []api.WakeJob{job}}
			hub := httptest.NewServer(h)
			defer hub.Close()
			c, _ := api.NewClient(hub.URL, time.Second)
			b.Hub = hub.URL
			var p relayProgress
			now := time.Now()
			var handled bool
			var brokerErr error
			checkLog(pass(func(ctx context.Context) error {
				handled, brokerErr = relayWakeJob(ctx, b, &p, c, now, nativeQueue)
				return nil
			}))
			h.mu.Lock()
			reports := append([]api.WakeJobReport(nil), h.reports...)
			if len(reports) == 0 {
				h.jobs = append(h.jobs, job)
			}
			h.mu.Unlock()
			if tc.queued {
				if !handled || brokerErr != nil || len(reports) != 1 || reports[0].Status != "accepted" {
					t.Fatalf("broker handled=%v err=%v reports=%+v, want one accepted", handled, brokerErr, reports)
				}
			} else if handled || brokerErr == nil || len(reports) != 1 || reports[0].Status != "failed" || !strings.Contains(reports[0].Detail, "Codex queue failed: "+tc.exit) {
				t.Fatalf("broker handled=%v err=%v reports=%+v, want one failed", handled, brokerErr, reports)
			}
			pass(func(ctx context.Context) error {
				_, err := relayWakeJob(ctx, b, &p, c, now.Add(time.Minute), nativeQueue)
				return err
			})
			if got := queueCalls(); got != 1 {
				t.Fatalf("broker path queued %d times, want 1", got)
			}

			// Inbox path.
			ih := &needsInputHub{t: t, agent: agent}
			ih.messages = []api.Message{{Seq: 41, To: agent.ID, From: api.Sender{Node: "workspace", User: "owner"}, Text: "owner input"}}
			inbox := httptest.NewServer(ih)
			defer inbox.Close()
			ic, _ := api.NewClient(inbox.URL, time.Second)
			b.Hub = inbox.URL
			var ip relayProgress
			var inboxErr error
			checkLog(pass(func(ctx context.Context) error {
				inboxErr = relayOne(ctx, b, &ip, ic, now, nativeQueue)
				return nil
			}))
			if tc.queued && (inboxErr != nil || ip.Through != 41) {
				t.Fatalf("inbox err=%v through=%d, want queued through 41", inboxErr, ip.Through)
			}
			if !tc.queued && (inboxErr == nil || !strings.Contains(inboxErr.Error(), "Codex queue failed: "+tc.exit) || ip.Through != 0) {
				t.Fatalf("inbox err=%v through=%d, want failed", inboxErr, ip.Through)
			}
			pass(func(ctx context.Context) error {
				_ = relayOne(ctx, b, &ip, ic, now.Add(time.Minute), nativeQueue)
				return nil
			})
			// A failed wake is retried; a queued one is not.
			want := 3
			if tc.queued {
				want = 2
			}
			if got := queueCalls(); got != want {
				t.Fatalf("queued %d times in all, want %d", got, want)
			}
		})
	}
	// A clean exit without the confirmation is still unconfirmed.
	fake := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'queued\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := nativeQueue(context.Background(), runtimeBinding{Agent: "agt_0123456789abcdef", Thread: "00000000-0000-4000-8000-000000000001", Codex: fake}, "wake"); err == nil || !strings.Contains(err.Error(), "Codex did not confirm a queued message") {
		t.Fatalf("clean exit without confirmation: %v", err)
	}
}

// rotationLoopFixture runs the relay loop against a local hub with one bound
// agent, away from the live hub, relay state, config and tmux socket. It
// counts the team queue polls and the bound agent's wake-pass reads.
type rotationLoopFixture struct {
	queuePolls, agentReads atomic.Int32
	// onAgentRead, when set, runs on each request for the bound agent.
	onAgentRead atomic.Pointer[func(*http.Request)]
}

// rotationLoopSentinel is private text the fixture serves as the agent's
// name; no relay log line may carry it.
const rotationLoopSentinel = "relay-private-sentinel-text"

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
			if hook := f.onAgentRead.Load(); hook != nil {
				(*hook)(r)
			}
			_ = json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, RunID: b.Run, Name: rotationLoopSentinel, Status: api.AgentDone, Online: true, Unread: 0})
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

// The loop's session cleanup and a rotation pass's cleanup of the rotated
// handler's old session must not stop one session at once (wi_63fa6bb934e0af46).
func TestRelayLoopCleanupWaitsForInFlightRotationPass(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	// rotating is set while a rotation pass runs its ticks.
	var rotating atomic.Bool
	var handlerCalls, cleanups, overlaps atomic.Int32
	handler := func(ctx context.Context) error {
		rotating.Store(true)
		if handlerCalls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil
	}
	steward := func(context.Context) error { rotating.Store(false); return nil }
	var passes int
	var cleanupsWhileBlocked int32
	var deadline time.Time
	pause := func() bool {
		passes++
		switch {
		case passes == 1:
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				t.Error("rotation tick never started")
				return false
			}
			return true
		case passes < 5:
			// Three more loop passes run while the rotation pass is held.
			return true
		case passes == 5:
			cleanupsWhileBlocked = cleanups.Load()
			unblock()
			deadline = time.Now().Add(15 * time.Second)
			return true
		}
		if cleanups.Load() > cleanupsWhileBlocked {
			return false
		}
		if time.Now().After(deadline) {
			t.Error("loop cleanup never ran after the rotation pass was collected")
			return false
		}
		time.Sleep(10 * time.Millisecond)
		return true
	}
	newRotationLoopFixture(t, handler, steward, pause)
	oldCleanup := relayLoopCleanup
	t.Cleanup(func() { relayLoopCleanup = oldCleanup })
	relayLoopCleanup = func() {
		cleanups.Add(1)
		if rotating.Load() {
			overlaps.Add(1)
		}
	}
	if err := cmdRelay(nil); err != nil {
		t.Fatal(err)
	}
	// Only the first pass, before the rotation pass started, cleans up.
	if cleanupsWhileBlocked != 1 {
		t.Fatalf("loop cleanups by the end of four passes with a rotation pass in flight = %d, want 1", cleanupsWhileBlocked)
	}
	if got := overlaps.Load(); got != 0 {
		t.Fatalf("loop cleanup ran %d time(s) while a rotation pass was in flight", got)
	}
	if cleanups.Load() <= cleanupsWhileBlocked {
		t.Fatalf("loop cleanups = %d, want one more after the rotation pass was collected", cleanups.Load())
	}
}

func TestRelayOnceWaitsForRotationPassBeforeWaking(t *testing.T) {
	var f *rotationLoopFixture
	var readsAtReturn atomic.Int32
	readsAtReturn.Store(-1)
	returned := make(chan struct{})
	// The handler tick holds for half a second, or ends early once a wake
	// pass reads the bound agent, as it would if once mode did not wait.
	handler := func(ctx context.Context) error {
		defer close(returned)
		hold := time.After(500 * time.Millisecond)
		for f.agentReads.Load() == 0 {
			select {
			case <-hold:
				readsAtReturn.Store(f.agentReads.Load())
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
		readsAtReturn.Store(f.agentReads.Load())
		return nil
	}
	steward := func(context.Context) error { return nil }
	pause := func() bool { t.Error("once mode paused between passes"); return false }
	f = newRotationLoopFixture(t, handler, steward, pause)
	if err := cmdRelay([]string{"--once"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(15 * time.Second):
		t.Fatal("rotation tick never returned")
	}
	if got := readsAtReturn.Load(); got != 0 {
		t.Fatalf("agent reads before the rotation pass returned = %d, want 0", got)
	}
	if f.agentReads.Load() == 0 {
		t.Fatal("once mode never ran the wake pass for the bound agent")
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
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var page []api.Message
		for _, m := range h.messages {
			if m.Seq > after && (limit <= 0 || len(page) < limit) {
				page = append(page, m)
			}
		}
		_ = json.NewEncoder(w).Encode(api.MessageList{Messages: page})
	case strings.HasSuffix(r.URL.Path, "/usage/holds"):
		// No team of this fixture is held past its token estimate.
		_ = json.NewEncoder(w).Encode(api.UsageHolds{Holds: []api.BudgetHold{}})
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

// A failed deployer read never holds the page's other wakes: the primary
// handler and the owner helper are still woken for a directed, broadcast or
// person's message beside the notice, and the cursor moves past both
// (wi_e58d6364a2789f9f).
func TestRelayDeployerNoticeFailedReadKeepsOtherWakes(t *testing.T) {
	owner := api.Sender{Node: "workspace", User: "owner"}
	others := []struct {
		name    string
		message func(agent string) api.Message
	}{
		{"directed", func(agent string) api.Message {
			return api.Message{Seq: 32, To: agent, From: owner, Text: "please look at the release"}
		}},
		{"broadcast", func(string) api.Message {
			return api.Message{Seq: 32, Broadcast: true, From: api.Sender{AgentID: noticeWorker, Node: "workspace", User: "owner"}, Text: "to everyone"}
		}},
		{"human", func(string) api.Message {
			return api.Message{Seq: 32, From: owner, Text: "a note from the owner"}
		}},
	}
	failures := []struct {
		name   string
		read   string
		status int
		agents []string
	}{
		{"author-500", noticeDeployer, http.StatusInternalServerError, noticeRecipients},
		{"author-429", noticeDeployer, http.StatusTooManyRequests, noticeRecipients},
		{"author-403", noticeDeployer, http.StatusForbidden, noticeRecipients},
		{"tasks-503", "tasks", http.StatusServiceUnavailable, []string{noticeHandler}},
	}
	for _, f := range failures {
		for _, agent := range f.agents {
			for _, other := range others {
				t.Run(f.name+"-"+agent+"-"+other.name, func(t *testing.T) {
					h, c, binding := newNoticeHub(t)
					h.messages = []api.Message{deployerNotice(31, noticeDeployer), other.message(agent)}
					h.fail[f.read] = f.status
					p := relayProgress{}
					wakes := 0
					queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
					if err := relayOne(context.Background(), binding(agent), &p, c, time.Now(), queue); err != nil || wakes != 1 || p.Through != 32 {
						t.Fatalf("err=%v wakes=%d progress=%+v", err, wakes, p)
					}
					if n := h.reads(f.read, false); n != 1 {
						t.Fatalf("the failing read was made %d times", n)
					}
				})
			}
		}
	}
}

// With only the notice on the page, a failed read is made again at most every
// 30 seconds, logs one skip for the episode, keeps the cursor below the notice
// and returns its error; the first pass after it recovers wakes once.
func TestRelayDeployerNoticeFailedReadSpacedAndLoggedOnce(t *testing.T) {
	for _, f := range []struct {
		read   string
		status int
	}{{noticeDeployer, http.StatusInternalServerError}, {"tasks", http.StatusServiceUnavailable}} {
		t.Run(f.read, func(t *testing.T) {
			h, c, binding := newNoticeHub(t)
			h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
			h.fail[f.read] = f.status
			p := relayProgress{}
			wakes := 0
			queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
			start := time.Now()
			pass := func(at time.Duration) error {
				return relayOne(context.Background(), binding(noticeHandler), &p, c, start.Add(at), queue)
			}
			out, _ := captureRelayOutput(t, true, func() error {
				for at := 0; at < 30; at += 3 {
					if err := pass(time.Duration(at) * time.Second); err == nil || wakes != 0 || p.Through != 0 {
						t.Errorf("pass at %ds: err=%v wakes=%d progress=%+v", at, err, wakes, p)
					}
				}
				if n := h.reads(f.read, false); n != 1 {
					t.Errorf("the failing read was made %d times in 30 seconds", n)
				}
				if p.Skip == nil || fmt.Sprint(p.Skip.MessageSeqs) != "[31]" {
					t.Errorf("skip: %+v", p.Skip)
				}
				if err := pass(31 * time.Second); err == nil || wakes != 0 || p.Through != 0 {
					t.Errorf("pass at 31s: err=%v wakes=%d progress=%+v", err, wakes, p)
				}
				if n := h.reads(f.read, false); n != 2 {
					t.Errorf("the failing read was made %d times after its retry delay", n)
				}
				return nil
			})
			if n := strings.Count(out, "wake skipped: deployer notice check failed"); n != 1 {
				t.Fatalf("%d skip lines for one episode:\n%s", n, out)
			}
			h.mu.Lock()
			delete(h.fail, f.read)
			h.mu.Unlock()
			if err := pass(62 * time.Second); err != nil || wakes != 1 || p.Through != 31 || p.Skip != nil {
				t.Fatalf("recovered read: err=%v wakes=%d progress=%+v", err, wakes, p)
			}
		})
	}
}

// A notice held by a lasting read failure behind a full page of the agent's
// own posts does not hide a later directed, broadcast or person's message: the
// first pass wakes for it, and over two minutes of 3-second passes the failing
// read is made once, as its 30-second spacing allows with the wake ending the
// hold (wi_b436f1129d2bb8cb).
func TestRelayDeployerNoticeHeldFullPageKeepsLaterWakes(t *testing.T) {
	owner := api.Sender{Node: "workspace", User: "owner"}
	others := []struct {
		name    string
		message func(agent string) api.Message
	}{
		{"directed", func(agent string) api.Message {
			return api.Message{Seq: 232, To: agent, From: owner, Text: "please look at the release"}
		}},
		{"broadcast", func(string) api.Message {
			return api.Message{Seq: 232, Broadcast: true, From: api.Sender{AgentID: noticeWorker, Node: "workspace", User: "owner"}, Text: "to everyone"}
		}},
		{"human", func(string) api.Message {
			return api.Message{Seq: 232, From: owner, Text: "a note from the owner"}
		}},
	}
	failures := []struct {
		name   string
		read   string
		status int
		agents []string
	}{
		{"author-403", noticeDeployer, http.StatusForbidden, noticeRecipients},
		{"tasks-503", "tasks", http.StatusServiceUnavailable, []string{noticeHandler}},
	}
	for _, f := range failures {
		for _, agent := range f.agents {
			for _, other := range others {
				t.Run(f.name+"-"+agent+"-"+other.name, func(t *testing.T) {
					h, c, binding := newNoticeHub(t)
					h.messages = append(heldNoticePage(agent, 31, 231), other.message(agent))
					h.fail[f.read] = f.status
					p := relayProgress{}
					var prompts []string
					queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
						prompts = append(prompts, prompt)
						return nil
					}
					start := time.Now()
					for at := 0; at < 120; at += 3 {
						if err := relayOne(context.Background(), binding(agent), &p, c, start.Add(time.Duration(at)*time.Second), queue); err != nil {
							t.Fatalf("pass at %ds: %v", at, err)
						}
						if len(prompts) != 1 || p.Through != 232 || !strings.Contains(prompts[0], "through message #232") {
							t.Fatalf("pass at %ds: prompts=%q progress=%+v", at, prompts, p)
						}
					}
					if n := h.reads(f.read, false); n != 1 {
						t.Fatalf("the failing read was made %d times in two minutes", n)
					}
				})
			}
		}
	}
}

// With nothing to wake for past a full held page, each pass looks one page
// further and the notice stays held, its failing read spaced as before; a
// message posted later still wakes, and once the read recovers the notice
// itself is decided and wakes (wi_b436f1129d2bb8cb).
func TestRelayDeployerNoticeHeldFullPageDecidedOnRecovery(t *testing.T) {
	h, c, binding := newNoticeHub(t)
	h.messages = heldNoticePage(noticeHandler, 31, 481)
	h.fail[noticeDeployer] = http.StatusForbidden
	p := relayProgress{}
	wakes := 0
	queue := func(context.Context, runtimeBinding, string) error { wakes++; return nil }
	start := time.Now()
	pass := func(p *relayProgress, at int) error {
		return relayOne(context.Background(), binding(noticeHandler), p, c, start.Add(time.Duration(at)*time.Second), queue)
	}
	for at := 0; at < 30; at += 3 {
		if err := pass(&p, at); err == nil || wakes != 0 || p.Through != 0 {
			t.Fatalf("pass at %ds: err=%v wakes=%d progress=%+v", at, err, wakes, p)
		}
	}
	if n := h.reads(noticeDeployer, false); n != 1 {
		t.Fatalf("the failing read was made %d times in 30 seconds", n)
	}
	if p.HeldPast != 481 || p.Skip == nil || fmt.Sprint(p.Skip.MessageSeqs) != "[31]" {
		t.Fatalf("the held page was not looked past to the end: progress=%+v skip=%+v", p, p.Skip)
	}
	// A message posted after the look reached the end still wakes.
	later := p
	h.mu.Lock()
	h.messages = append(h.messages, api.Message{Seq: 482, To: noticeHandler, From: api.Sender{Node: "workspace", User: "owner"}, Text: "please look"})
	h.mu.Unlock()
	if err := pass(&later, 30); err != nil || wakes != 1 || later.Through != 482 {
		t.Fatalf("later message: err=%v wakes=%d progress=%+v", err, wakes, later)
	}
	h.mu.Lock()
	h.messages = h.messages[:len(h.messages)-1]
	delete(h.fail, noticeDeployer)
	h.mu.Unlock()
	// Without it, the first pass after the read recovers wakes for the notice.
	wakes = 0
	if err := pass(&p, 61); err != nil || wakes != 1 || p.Through != 230 || p.HeldPast != 0 {
		t.Fatalf("recovered read: err=%v wakes=%d progress=%+v", err, wakes, p)
	}
}

// heldNoticePage is a deployer notice at seq followed by the agent's own
// posts through last, which never wake it.
func heldNoticePage(agent string, seq, last int64) []api.Message {
	page := []api.Message{deployerNotice(seq, noticeDeployer)}
	for s := seq + 1; s <= last; s++ {
		page = append(page, api.Message{Seq: s, From: api.Sender{AgentID: agent, Node: "workspace", User: "owner"}, Text: "own post"})
	}
	return page
}

// A failing task list read is made once per 30 seconds for all of a project's
// handler bindings, and each handler is still woken for its own message.
func TestRelayDeployerNoticeTaskListFailureSpacedAcrossHandlers(t *testing.T) {
	h, c, binding := newNoticeHub(t)
	handlers := []string{noticeHandler, noticeHandlerB, "agt_00000000000000e1", "agt_00000000000000e2", "agt_00000000000000e3", "agt_00000000000000e4"}
	for _, id := range handlers[2:] {
		h.agents[id] = api.Agent{ID: id, RunID: noticeRun, Role: api.AgentRoleDatabaseHandler, Runtime: "codex", Status: api.AgentDone, Online: true, Unread: 1}
	}
	h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	for i, id := range handlers {
		h.messages = append(h.messages, api.Message{Seq: int64(32 + i), To: id, From: api.Sender{Node: "workspace", User: "owner"}, Text: "a request for this handler"})
	}
	h.fail["tasks"] = http.StatusServiceUnavailable
	woken := map[string]int{}
	queue := func(_ context.Context, b runtimeBinding, _ string) error {
		woken[b.Agent]++
		return nil
	}
	progress := map[string]*relayProgress{}
	for _, id := range handlers {
		progress[id] = &relayProgress{}
	}
	start := time.Now()
	for at := 0; at < 30; at += 3 {
		for _, id := range handlers {
			if err := relayOne(context.Background(), binding(id), progress[id], c, start.Add(time.Duration(at)*time.Second), queue); err != nil {
				t.Fatalf("%s at %ds: %v", id, at, err)
			}
		}
	}
	if n := h.reads("tasks", false); n != 1 {
		t.Fatalf("six handler bindings read the failing task list %d times in 30 seconds", n)
	}
	for _, id := range handlers {
		if woken[id] != 1 || progress[id].Through != 37 {
			t.Fatalf("%s: woken=%v progress=%+v", id, woken, progress[id])
		}
	}
	if n := h.reads(noticeDeployer, false); n != 0 {
		t.Fatalf("the deployer was read %d times with no primary known", n)
	}
}

// A deployer notice posted after a committed handler rotation wakes the new
// primary on its first pass, even while the old primary's read is still
// cached, and does not wake the old primary.
func TestRelayDeployerNoticeAfterRotationWithinCacheMinute(t *testing.T) {
	h, c, binding := newNoticeHub(t)
	h.messages = []api.Message{deployerNotice(31, noticeDeployer)}
	woken := map[string]int{}
	queue := func(_ context.Context, b runtimeBinding, _ string) error {
		woken[b.Agent]++
		return nil
	}
	progress := map[string]*relayProgress{noticeHandler: {}, noticeHandlerB: {}}
	start := time.Now()
	for _, agent := range []string{noticeHandler, noticeHandlerB} {
		if err := relayOne(context.Background(), binding(agent), progress[agent], c, start, queue); err != nil {
			t.Fatal(err)
		}
	}
	if woken[noticeHandler] != 1 || len(woken) != 1 {
		t.Fatalf("before the rotation: woken=%v", woken)
	}
	h.mu.Lock()
	h.primary = noticeHandlerB
	h.messages = append(h.messages, deployerNotice(32, noticeDeployer))
	h.mu.Unlock()
	for _, agent := range []string{noticeHandlerB, noticeHandler} {
		if err := relayOne(context.Background(), binding(agent), progress[agent], c, start.Add(20*time.Second), queue); err != nil {
			t.Fatal(err)
		}
	}
	if woken[noticeHandlerB] != 1 || woken[noticeHandler] != 1 {
		t.Fatalf("after the rotation: woken=%v", woken)
	}
	if progress[noticeHandler].Through != 32 || progress[noticeHandlerB].Through != 32 {
		t.Fatalf("progress: %+v %+v", progress[noticeHandler], progress[noticeHandlerB])
	}
	if n := h.reads("tasks", false); n != 2 {
		t.Fatalf("the task list was read %d times for two notices", n)
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

// holdCalls counts the holds reads and wake-job leases among hub calls.
func holdCalls(calls []string) (holds, leases int) {
	for _, call := range calls {
		if strings.HasSuffix(call, "/usage/holds") {
			holds++
		}
		if strings.HasSuffix(call, "/wake-jobs/lease") {
			leases++
		}
	}
	return holds, leases
}

// a14, relay half: the broker path makes no holds read, because the hub
// leases nothing for a held run. The inbox path reads the hold fresh, for the
// binding's exact run, immediately before a delivery, and withholds it. A
// different run, an agent with a project role and a hub without the route
// are delivered as before; a pass with nothing to deliver reads nothing.
func TestRelayHoldGate(t *testing.T) {
	b, hub, c, _ := needsInputFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)
	var queued []string
	queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
		queued = append(queued, prompt)
		return nil
	}
	owner := api.Sender{Node: "workspace", User: "owner"}
	seq := int64(40)
	// unread gives the agent one new directed message, and due makes the
	// inbox and broker paths due again.
	unread := func() {
		seq++
		hub.update(func(h *needsInputHub) {
			h.agent.ReadUpTo, h.agent.Unread = seq-1, 1
			h.messages = append(h.messages, api.Message{Seq: seq, To: b.Agent, From: owner})
		})
		now = now.Add(time.Minute)
	}
	job := func(id string) {
		hub.update(func(h *needsInputHub) {
			h.jobs = append(h.jobs, api.WakeJob{ID: id, MessageSeq: seq, Prompt: "Tailterm obligation #" + strconv.FormatInt(seq, 10), LeaseToken: "lease-" + id})
		})
	}
	var p relayProgress

	// Starting with no hold: one holds read, then delivery.
	unread()
	hub.takeCalls()
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || len(queued) != 1 || p.TeamHeld || p.Skip != nil {
		t.Fatalf("no hold: %d holds reads, queued %q, skip %+v", holds, queued, p.Skip)
	}
	if !relayStallPassDue(b, p) {
		t.Fatal("the stall pass is off for a binding that is not held")
	}

	// A hold is written. The very next inbox attempt delivers nothing after
	// exactly one holds read, and records the skip with its reason.
	hub.update(func(h *needsInputHub) { h.heldRun = b.Run })
	unread()
	wakes := p.Wakes
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	calls := hub.takeCalls()
	if holds, _ := holdCalls(calls); holds != 1 || len(queued) != 1 || !p.TeamHeld || !strings.HasSuffix(calls[len(calls)-1], "/usage/holds") {
		t.Fatalf("held inbox attempt: calls %v, queued %q", calls, queued)
	}
	if p.Skip == nil || p.Skip.Reason != relayTeamHeldReason || fmt.Sprint(p.Skip.MessageSeqs) != fmt.Sprint([]int64{seq}) || p.Wakes != wakes || p.Through >= seq {
		t.Fatalf("held skip %+v wakes %d through %d", p.Skip, p.Wakes, p.Through)
	}
	// The stall pass is not run for a held binding.
	if relayStallPassDue(b, p) {
		t.Fatal("the stall pass would run for a held binding")
	}
	other := b
	other.Run = "run_0000000000000002"
	if !relayStallPassDue(other, p) {
		t.Fatal("the stall pass is off for another run of the agent")
	}

	// The next inbox attempt for the held binding waits a minute: inside it,
	// with the message still unread, no hub read past the agent's own is made.
	if !p.NextHeldAttempt.Equal(now.Add(relayHeldRetry)) || relayHeldRetry < time.Minute {
		t.Fatalf("next attempt after a held result at %s, want %s", p.NextHeldAttempt, now.Add(relayHeldRetry))
	}
	for _, wait := range []time.Duration{16 * time.Second, 45 * time.Second, 59 * time.Second} {
		if err := relayOne(ctx, b, &p, c, now.Add(wait), queue); err != nil {
			t.Fatal(err)
		}
		calls = hub.takeCalls()
		if holds, _ := holdCalls(calls); holds != 0 || len(calls) != 2 || len(queued) != 1 || p.Wakes != wakes {
			t.Fatalf("%s after a held result: calls %v, queued %d, wakes %d", wait, calls, len(queued), p.Wakes)
		}
	}
	// At the minute it asks again, once, and is withheld again.
	now = now.Add(relayHeldRetry)
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || len(queued) != 1 || p.Wakes != wakes || !p.NextHeldAttempt.Equal(now.Add(relayHeldRetry)) {
		t.Fatalf("a minute after a held result: %d holds reads, queued %d, wakes %d", holds, len(queued), p.Wakes)
	}
	// Continue: the very next attempt delivers the withheld message, and the
	// spacing is gone. The hold is then written again for the cases below.
	hub.update(func(h *needsInputHub) { h.heldRun = "" })
	now = now.Add(relayHeldRetry)
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || len(queued) != 2 || p.TeamHeld || !p.NextHeldAttempt.IsZero() || p.Skip != nil || p.Through != seq {
		t.Fatalf("first attempt after continue: %d holds reads, queued %d, through %d", holds, len(queued), p.Through)
	}
	queued = queued[:1]
	hub.update(func(h *needsInputHub) { h.heldRun = b.Run })
	unread()
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil || !p.TeamHeld || len(queued) != 1 {
		t.Fatalf("held again: %v queued %d", err, len(queued))
	}
	hub.takeCalls()

	// The broker path makes no holds read: it asks for a lease as always, and
	// the hub leases nothing for the held run.
	job("wake_1")
	now = now.Add(time.Minute)
	if handled, err := relayWakeJob(ctx, b, &p, c, now, queue); err != nil || handled {
		t.Fatalf("held broker attempt: %v %v", handled, err)
	}
	calls = hub.takeCalls()
	if holds, leases := holdCalls(calls); holds != 0 || leases != 1 || len(queued) != 1 || len(hub.jobs) != 1 || !p.TeamHeld {
		t.Fatalf("held broker attempt: calls %v, queued %q, jobs %d", calls, queued, len(hub.jobs))
	}

	// A pass with nothing to deliver makes no holds read: the broker path is
	// not due and the agent has nothing unread.
	hub.update(func(h *needsInputHub) { h.agent.Unread = 0 })
	if err := relayPass(b, &p, c, now.Add(time.Second), queue); err != nil {
		t.Fatal(err)
	}
	if holds, leases := holdCalls(hub.takeCalls()); holds != 0 || leases != 0 || len(queued) != 1 {
		t.Fatalf("an idle pass made %d holds reads and %d leases", holds, leases)
	}
	// Nor does one inside the 15-second inbox spacing.
	hub.update(func(h *needsInputHub) { h.agent.Unread = 1 })
	p.LastAttempt = now
	if err := relayOne(ctx, b, &p, c, now.Add(5*time.Second), queue); err != nil {
		t.Fatal(err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 0 {
		t.Fatalf("a deferred inbox pass made %d holds reads", holds)
	}

	// A failing holds read skips the inbox pass: nothing is delivered.
	hub.update(func(h *needsInputHub) { h.holdsStatus = http.StatusInternalServerError })
	now = now.Add(time.Minute)
	if err := relayOne(ctx, b, &p, c, now, queue); err == nil {
		t.Fatal("a failing holds read did not fail the inbox pass")
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || len(queued) != 1 {
		t.Fatalf("a failing holds read: %d reads, queued %q", holds, queued)
	}
	hub.update(func(h *needsInputHub) { h.holdsStatus = 0 })

	// Continue: the hold is lifted, and the next attempt leases and delivers.
	hub.update(func(h *needsInputHub) { h.heldRun = "" })
	now = now.Add(time.Minute)
	if handled, err := relayWakeJob(ctx, b, &p, c, now, queue); err != nil || !handled {
		t.Fatalf("broker attempt after continue: %v %v", handled, err)
	}
	calls = hub.takeCalls()
	if holds, leases := holdCalls(calls); holds != 0 || leases != 1 || len(queued) != 2 || !strings.Contains(queued[1], "wake_1") || len(hub.reports) != 1 || hub.reports[0].Status != "accepted" {
		t.Fatalf("after continue: calls %v, queued %q, reports %+v", calls, queued, hub.reports)
	}
	// The inbox path delivers again too, and clears the skip and the
	// remembered hold.
	p.BrokerWakes = false
	now = now.Add(time.Minute)
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || len(queued) != 3 || p.Skip != nil || p.TeamHeld {
		t.Fatalf("inbox attempt after continue: %d holds reads, queued %d, skip %+v", holds, len(queued), p.Skip)
	}
	if !relayStallPassDue(b, p) {
		t.Fatal("the stall pass stays off after the hold is lifted")
	}

	// The hold names another run of this agent: this binding is not held.
	hub.update(func(h *needsInputHub) { h.heldRun = "run_0000000000000002" })
	unread()
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || len(queued) != 4 || p.TeamHeld {
		t.Fatalf("a hold on another run: %d holds reads, queued %d", holds, len(queued))
	}

	// An agent with a project role is woken and causes no holds read, even
	// when the hub would list its run.
	hub.update(func(h *needsInputHub) { h.heldRun = b.Run })
	for _, role := range []string{api.AgentRoleDatabaseHandler, api.AgentRoleBacklogSteward, api.AgentRoleDeployment, api.AgentRoleOwnerHelper} {
		hub.update(func(h *needsInputHub) { h.agent.Role = role })
		unread()
		job("wake_" + role)
		before := len(queued)
		if handled, err := relayWakeJob(ctx, b, &p, c, now, queue); err != nil || !handled {
			t.Fatalf("%s broker attempt: %v %v", role, handled, err)
		}
		p.BrokerWakes = false
		now = now.Add(time.Minute)
		if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
			t.Fatalf("%s inbox attempt: %v", role, err)
		}
		if holds, leases := holdCalls(hub.takeCalls()); holds != 0 || leases != 1 || len(queued) != before+2 || p.TeamHeld {
			t.Fatalf("%s: %d holds reads, %d leases, queued %d of %d", role, holds, leases, len(queued)-before, 2)
		}
	}
	// So does a binding that carries the role itself.
	hub.update(func(h *needsInputHub) { h.agent.Role = "" })
	if held, err := relayTeamHeld(ctx, c, runtimeBinding{Hub: b.Hub, Task: b.Task, Agent: b.Agent, Run: b.Run, Role: api.AgentRoleOwnerHelper}, hub.agent); err != nil || held {
		t.Fatalf("a role binding: %v %v", held, err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 0 {
		t.Fatalf("a role binding made %d holds reads", holds)
	}

	// A hub without the holds route wakes as before.
	hub.update(func(h *needsInputHub) { h.noHolds = true })
	unread()
	job("wake_older")
	before := len(queued)
	if handled, err := relayWakeJob(ctx, b, &p, c, now, queue); err != nil || !handled {
		t.Fatalf("older hub broker attempt: %v %v", handled, err)
	}
	p.BrokerWakes = false
	now = now.Add(time.Minute)
	if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
		t.Fatalf("older hub inbox attempt: %v", err)
	}
	if _, leases := holdCalls(hub.takeCalls()); leases != 1 || len(queued) != before+2 || p.TeamHeld {
		t.Fatalf("older hub: %d leases, queued %d", leases, len(queued)-before)
	}
}

// r1 (wi_f9d82dcb91a5e002): a binding remembered as held whose agent then has
// nothing unread, because its running turn read the withheld message, is
// asked about again at most every five minutes. The first answer that says
// not held forgets the hold, so the stall pass runs again; nothing is queued
// by that read, and no read is made once the hold is forgotten.
func TestRelayHoldRecheck(t *testing.T) {
	b, hub, c, _ := needsInputFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 16, 0, 0, 0, time.UTC)
	var queued []string
	queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
		queued = append(queued, prompt)
		return nil
	}
	var p relayProgress
	// pass runs one inbox pass and returns its holds reads.
	pass := func(what string) int {
		t.Helper()
		if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		holds, _ := holdCalls(hub.takeCalls())
		return holds
	}
	// The team is held and one message is withheld.
	hub.update(func(h *needsInputHub) {
		h.heldRun = b.Run
		h.agent.ReadUpTo, h.agent.Unread = 40, 1
		h.messages = append(h.messages, api.Message{Seq: 41, To: b.Agent, From: api.Sender{Node: "workspace", User: "owner"}})
	})
	hub.takeCalls()
	if holds := pass("withheld attempt"); holds != 1 || !p.TeamHeld || !p.HeldChecked.Equal(now) || p.Skip == nil || relayStallPassDue(b, p) {
		t.Fatalf("withheld attempt: %d holds reads, progress %+v", holds, p)
	}
	withheld := now
	// The agent's running turn reads the message, and the hold stays.
	hub.update(func(h *needsInputHub) { h.agent.ReadUpTo, h.agent.Unread = 41, 0 })
	for _, wait := range []time.Duration{time.Second, time.Minute, relayHeldRecheck - time.Second} {
		now = withheld.Add(wait)
		if holds := pass("an empty inbox " + wait.String() + " after the holds read"); holds != 0 || !p.TeamHeld || relayStallPassDue(b, p) {
			t.Fatalf("%s after the holds read: %d holds reads, held %v", wait, holds, p.TeamHeld)
		}
	}
	// At five minutes: one read. Still held, so nothing else changes, and
	// no further read is made for another five minutes.
	now = withheld.Add(relayHeldRecheck)
	if holds := pass("the first re-check"); holds != 1 || !p.TeamHeld || relayStallPassDue(b, p) || p.Skip == nil || p.Skip.Reason != relayTeamHeldReason || !p.HeldChecked.Equal(now) {
		t.Fatalf("the first re-check, still held: %d holds reads, progress %+v", holds, p)
	}
	checked := now
	for _, wait := range []time.Duration{3 * time.Second, time.Minute, relayHeldRecheck - time.Second} {
		now = checked.Add(wait)
		if holds := pass("an empty inbox " + wait.String() + " after the re-check"); holds != 0 || !p.TeamHeld {
			t.Fatalf("%s after a re-check that said held: %d holds reads, held %v", wait, holds, p.TeamHeld)
		}
	}
	// The owner continues the hold. Before the next re-check is due the
	// relay has not asked, so the stall pass is still off.
	hub.update(func(h *needsInputHub) { h.heldRun = "" })
	if holds := pass("continued, before the re-check is due"); holds != 0 || relayStallPassDue(b, p) {
		t.Fatalf("continued, before the re-check is due: %d holds reads, stall pass due %v", holds, relayStallPassDue(b, p))
	}
	// A failed read counts as made: the next one waits five minutes.
	hub.update(func(h *needsInputHub) { h.holdsStatus = http.StatusInternalServerError })
	now = checked.Add(relayHeldRecheck)
	if err := relayOne(ctx, b, &p, c, now, queue); err == nil {
		t.Fatal("a failing re-check did not fail the pass")
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || !p.TeamHeld {
		t.Fatalf("a failing re-check: %d holds reads, held %v", holds, p.TeamHeld)
	}
	hub.update(func(h *needsInputHub) { h.holdsStatus = 0 })
	failed := now
	now = failed.Add(relayHeldRecheck - time.Second)
	if holds := pass("inside five minutes of a failed re-check"); holds != 0 || !p.TeamHeld {
		t.Fatalf("inside five minutes of a failed re-check: %d holds reads, held %v", holds, p.TeamHeld)
	}
	// At five minutes: exactly one read, the hold is forgotten with its
	// skip and spacing, the stall pass is due, and nothing was queued.
	now = failed.Add(relayHeldRecheck)
	if holds := pass("the re-check after continue"); holds != 1 || p.TeamHeld || !p.NextHeldAttempt.IsZero() || !p.HeldChecked.IsZero() || p.Skip != nil || !relayStallPassDue(b, p) || len(queued) != 0 {
		t.Fatalf("the re-check after continue: %d holds reads, queued %q, progress %+v", holds, queued, p)
	}
	// It stops there: an empty inbox makes no holds read again, however long.
	for _, wait := range []time.Duration{time.Second, relayHeldRecheck, 3 * relayHeldRecheck} {
		now = now.Add(wait)
		if holds := pass("an empty inbox after the hold is forgotten"); holds != 0 || p.TeamHeld {
			t.Fatalf("%s after the hold is forgotten: %d holds reads", wait, holds)
		}
	}

	// A progress file saved before this field existed has no check time: a
	// binding saved as held is asked about once, at once, then spaced.
	old := relayProgress{Run: b.Run, Thread: b.Thread, TeamHeld: true}
	hub.update(func(h *needsInputHub) { h.heldRun = b.Run })
	for i, want := range []int{1, 0} {
		now = now.Add(time.Second)
		if err := relayOne(ctx, b, &old, c, now, queue); err != nil {
			t.Fatal(err)
		}
		if holds, _ := holdCalls(hub.takeCalls()); holds != want || !old.TeamHeld {
			t.Fatalf("pass %d on an older progress file: %d holds reads, want %d", i, holds, want)
		}
	}
	// An agent with a project role is never held: the hold is forgotten
	// with no read.
	hub.update(func(h *needsInputHub) { h.agent.Role = api.AgentRoleDatabaseHandler })
	now = now.Add(relayHeldRecheck)
	if err := relayOne(ctx, b, &old, c, now, queue); err != nil {
		t.Fatal(err)
	}
	if holds, _ := holdCalls(hub.takeCalls()); holds != 0 || old.TeamHeld || len(queued) != 0 {
		t.Fatalf("a role agent remembered as held: %d holds reads, held %v, queued %q", holds, old.TeamHeld, queued)
	}
}

// h1, h2 (wi_42d04ab3c5f392df): a binding remembered as held whose agent has
// unread mail, none of which the inbox path delivers, makes no inbox attempt,
// so the same spaced read asks about the hold: none before five minutes, one
// at five minutes, a failed read spaced the same way, and the first answer
// that says not held turns the stall pass back on. The read queues nothing,
// and progress through the page is saved, also when the read fails.
func TestRelayHoldRecheckWithOnlyUndeliveredUnread(t *testing.T) {
	const teammate = "agt_00000000000000bb"
	for _, tc := range []struct {
		name string
		// broker: the binding's obligations are woken by broker wake jobs.
		broker bool
		unread func(b runtimeBinding) []api.Message
	}{
		{"own post and hub notice", false, func(b runtimeBinding) []api.Message {
			return []api.Message{
				{Seq: 42, From: api.Sender{AgentID: b.Agent}, Text: "Progress posted by the held agent"},
				{Seq: 43, To: b.Agent, From: api.Sender{Node: api.BrokerNode}, Text: "A hub notice"},
			}
		}},
		{"broker-covered kind", true, func(b runtimeBinding) []api.Message {
			return []api.Message{
				{Seq: 42, To: b.Agent, From: api.Sender{AgentID: teammate}, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, Subject: "A request the broker wake job delivers"}},
				{Seq: 43, To: b.Agent, From: api.Sender{AgentID: teammate}, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "A notice the broker wake job delivers"}},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, hub, c, _ := needsInputFixture(t)
			ctx := context.Background()
			now := time.Date(2026, 10, 9, 19, 0, 0, 0, time.UTC)
			var queued []string
			queue := func(_ context.Context, _ runtimeBinding, prompt string) error {
				queued = append(queued, prompt)
				return nil
			}
			p := relayProgress{Run: b.Run, Thread: b.Thread, BrokerWakes: tc.broker}
			// pass runs one inbox pass and returns its holds reads.
			pass := func(what string) int {
				t.Helper()
				if err := relayOne(ctx, b, &p, c, now, queue); err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				holds, _ := holdCalls(hub.takeCalls())
				return holds
			}
			// The team is held and one message is withheld.
			hub.update(func(h *needsInputHub) {
				h.heldRun = b.Run
				h.agent.ReadUpTo, h.agent.Unread = 40, 1
				h.messages = append(h.messages, api.Message{Seq: 41, To: b.Agent, From: api.Sender{Node: "workspace", User: "owner"}})
			})
			hub.takeCalls()
			if holds := pass("withheld attempt"); holds != 1 || !p.TeamHeld || p.Skip == nil || p.Skip.Reason != relayTeamHeldReason || relayStallPassDue(b, p) || p.Through >= 41 {
				t.Fatalf("withheld attempt: %d holds reads, progress %+v", holds, p)
			}
			withheld := now
			// The agent's running turn reads the withheld message. What is
			// unread after it is only mail the inbox path does not deliver.
			hub.update(func(h *needsInputHub) {
				h.agent.ReadUpTo, h.agent.Unread = 41, 2
				h.messages = append(h.messages, tc.unread(b)...)
			})
			for _, wait := range []time.Duration{time.Second, relayHeldRetry, relayHeldRecheck - time.Second} {
				now = withheld.Add(wait)
				if holds := pass(wait.String() + " after the holds read"); holds != 0 || !p.TeamHeld || relayStallPassDue(b, p) || len(queued) != 0 {
					t.Fatalf("%s after the holds read: %d holds reads, held %v, queued %q", wait, holds, p.TeamHeld, queued)
				}
			}
			// The page was read and found to hold nothing to deliver: progress
			// through it is saved without a holds read.
			if p.Through != 43 {
				t.Fatalf("progress through the page = %d, want 43", p.Through)
			}
			// At five minutes: one read. Still held, so the stall pass stays
			// off and no further read is made for another five minutes.
			now = withheld.Add(relayHeldRecheck)
			if holds := pass("the first re-check"); holds != 1 || !p.TeamHeld || relayStallPassDue(b, p) || p.Skip == nil || p.Skip.Reason != relayTeamHeldReason || !p.HeldChecked.Equal(now) {
				t.Fatalf("the first re-check, still held: %d holds reads, progress %+v", holds, p)
			}
			checked := now
			for _, wait := range []time.Duration{time.Minute, relayHeldRecheck - time.Second} {
				now = checked.Add(wait)
				if holds := pass(wait.String() + " after the re-check"); holds != 0 || !p.TeamHeld {
					t.Fatalf("%s after a re-check that said held: %d holds reads, held %v", wait, holds, p.TeamHeld)
				}
			}
			// The owner continues the hold. Before the next re-check is due
			// the relay has not asked, so the stall pass is still off.
			hub.update(func(h *needsInputHub) { h.heldRun = "" })
			if holds := pass("continued, before the re-check is due"); holds != 0 || relayStallPassDue(b, p) {
				t.Fatalf("continued, before the re-check is due: %d holds reads, stall pass due %v", holds, relayStallPassDue(b, p))
			}
			// A failed read counts as made, and the page read before it is
			// not lost: one more own post arrives and progress covers it.
			hub.update(func(h *needsInputHub) {
				h.holdsStatus = http.StatusInternalServerError
				h.agent.Unread = 3
				h.messages = append(h.messages, api.Message{Seq: 44, From: api.Sender{AgentID: b.Agent}, Text: "Another post by the held agent"})
			})
			now = checked.Add(relayHeldRecheck)
			if err := relayOne(ctx, b, &p, c, now, queue); err == nil {
				t.Fatal("a failing re-check did not fail the pass")
			}
			if holds, _ := holdCalls(hub.takeCalls()); holds != 1 || !p.TeamHeld || p.Through != 44 {
				t.Fatalf("a failing re-check: %d holds reads, held %v, through %d", holds, p.TeamHeld, p.Through)
			}
			hub.update(func(h *needsInputHub) { h.holdsStatus = 0 })
			failed := now
			now = failed.Add(relayHeldRecheck - time.Second)
			if holds := pass("inside five minutes of a failed re-check"); holds != 0 || !p.TeamHeld || relayStallPassDue(b, p) {
				t.Fatalf("inside five minutes of a failed re-check: %d holds reads, held %v", holds, p.TeamHeld)
			}
			// At five minutes: exactly one read, the hold is forgotten with
			// its skip and spacing, the stall pass is due, nothing was queued.
			now = failed.Add(relayHeldRecheck)
			if holds := pass("the re-check after continue"); holds != 1 || p.TeamHeld || !p.NextHeldAttempt.IsZero() || !p.HeldChecked.IsZero() || p.Skip != nil || !relayStallPassDue(b, p) || len(queued) != 0 || p.Through != 44 {
				t.Fatalf("the re-check after continue: %d holds reads, queued %q, progress %+v", holds, queued, p)
			}
			// It stops there: the same unread mail makes no holds read again.
			for _, wait := range []time.Duration{time.Second, relayHeldRecheck, 3 * relayHeldRecheck} {
				now = now.Add(wait)
				if holds := pass("after the hold is forgotten"); holds != 0 || p.TeamHeld || len(queued) != 0 {
					t.Fatalf("%s after the hold is forgotten: %d holds reads, queued %q", wait, holds, queued)
				}
			}
		})
	}
}

// startupOutageHub is a fake hub that does not answer until up is set: each
// request waits past the bind deadline. Then it answers for one agent with
// the given state and one directed message, and records any path it does
// not model rather than failing on it.
type startupOutageHub struct {
	up      atomic.Bool
	agent   atomic.Pointer[api.Agent]
	message api.Message
	mu      sync.Mutex
	other   []string
}

func (h *startupOutageHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.up.Load() {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		return
	}
	a := *h.agent.Load()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/team-queues":
		_, _ = w.Write([]byte(`{"entries":[]}`))
	case strings.HasSuffix(r.URL.Path, "/pause"):
		_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
	case strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(api.MessageList{Messages: []api.Message{h.message}})
	case r.URL.Path == "/v1/tasks/"+a.TaskID+"/agents/"+a.ID && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(a)
	default:
		h.mu.Lock()
		h.other = append(h.other, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		http.NotFound(w, r)
	}
}

func startupOutageFixture(t *testing.T) (*startupOutageHub, env, string, string) {
	t.Helper()
	stateDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", stateDir)
	t.Setenv("TT_TMUX_SOCKET", "relay-startup-bind-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	for _, name := range []string{"TAILTERM_TOKEN", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN", "CODEX_HOME"} {
		t.Setenv(name, "")
	}
	bin := t.TempDir()
	queued := filepath.Join(stateDir, "queued.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + queued + "'\nprintf 'Queued message\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	thread := "00000000-0000-4000-8000-0000000000a1"
	t.Setenv("CODEX_THREAD_ID", thread)
	t.Setenv(spawn.EnvSession, "tt-startup-bind")
	previous := runtimeBindTimeout
	runtimeBindTimeout = 150 * time.Millisecond
	t.Cleanup(func() { runtimeBindTimeout = previous })
	a := api.Agent{ID: "agt_00000000000000a1", TaskID: "tsk_00000000000000a1", Name: "db-handler-test", RunID: "run_00000000000000a1", Runtime: "codex", Status: api.AgentDone, Online: true, Unread: 1, Session: "tt-startup-bind", Cwd: stateDir}
	h := &startupOutageHub{message: api.Message{Seq: 40, TaskID: a.TaskID, To: a.ID, From: api.Sender{AgentID: "agt_00000000000000b2", Node: "workspace", User: "owner"}, Text: "REQUEST: file a bug"}}
	h.agent.Store(&a)
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	t.Setenv("TAILTERM_HUB", server.URL)
	return h, env{hub: server.URL, task: a.TaskID, agent: a.ID, runID: a.RunID}, thread, queued
}

func relayTestClient(hub string) (*api.Client, error) { return api.NewClient(hub, runtimeBindTimeout) }

// b1/b3: a Codex agent whose first tt commands all time out against the hub
// saves its binding as pending and ends its turn unbound. Once the hub
// answers, one relay pass binds the exact thread and the next directed
// message wakes the agent, with no command from it and no manual input.
func TestRelayBindsCodexAgentAfterStartupOutage(t *testing.T) {
	h, e, thread, queued := startupOutageFixture(t)
	for _, command := range []string{"status", "agents", "inbox", "event", "post"} {
		out, _ := captureRelayOutput(t, true, func() error { autoBindRuntime(e, command); return nil })
		if !strings.Contains(out, "could not bind yet") {
			t.Fatalf("%s: auto-bind output %q", command, out)
		}
	}
	if _, ok := readBinding(t, e.hub, e.agent); ok {
		t.Fatal("binding written without a hub confirmation")
	}
	pendingPath := pendingBindingPath(runtimeBinding{Hub: e.hub, Agent: e.agent})
	var pending runtimeBinding
	if data, err := os.ReadFile(pendingPath); err != nil || json.Unmarshal(data, &pending) != nil {
		t.Fatalf("pending binding not saved: %v", err)
	}
	if pending.Thread != thread || pending.Run != e.runID || pending.Task != e.task || pending.Runtime != "codex" || pending.Role != "" {
		t.Fatalf("pending binding %+v", pending)
	}
	if info, err := os.Stat(pendingPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("pending binding mode %v %v", info, err)
	}
	// The hub still does not answer: the relay keeps the record, writes nothing.
	retryPendingBindings(context.Background(), relayDir(), time.Now(), relayTestClient)
	if _, ok := readBinding(t, e.hub, e.agent); ok {
		t.Fatal("relay bound while the hub was silent")
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("pending binding dropped during the outage: %v", err)
	}
	h.up.Store(true)
	out, err := captureRelayOutput(t, true, func() error { return cmdRelay([]string{"--once"}) })
	if err != nil {
		t.Fatalf("relay pass: %v\n%s", err, out)
	}
	b, ok := readBinding(t, e.hub, e.agent)
	if !ok || b.Thread != thread || b.Run != e.runID || b.Task != e.task || b.Runtime != "codex" || b.Role != "" || b.Session != "tt-startup-bind" || b.Cwd == "" || !validBinding(b) {
		t.Fatalf("binding after the hub answered: %+v (ok=%v)\n%s", b, ok, out)
	}
	if !strings.Contains(out, "bound after its startup bind went unanswered") {
		t.Fatalf("relay log missing the startup bind line:\n%s", out)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending binding kept after binding: %v", err)
	}
	data, err := os.ReadFile(queued)
	if err != nil {
		t.Fatalf("directed message did not wake the agent: %v\nrelay:\n%s\nunmodeled: %v", err, out, h.other)
	}
	if got := string(data); !strings.Contains(got, "queue --thread "+thread) || !strings.Contains(got, "through message #40") {
		t.Fatalf("wake queued %q", got)
	}
}

// a2: the pending path writes no binding for an owner helper, a superseded
// run, a closed, exited or retired agent, a non-Codex runtime, a hub that
// refuses the agent, or an expired record, and it drops the record. An owner
// helper whose registration binding exists saves no pending record at all.
func TestRelayPendingBindingRefusals(t *testing.T) {
	h, e, thread, _ := startupOutageFixture(t)
	base := *h.agent.Load()
	pendingPath := pendingBindingPath(runtimeBinding{Hub: e.hub, Agent: e.agent})
	cases := []struct {
		name  string
		agent func(api.Agent) api.Agent
		age   time.Duration
	}{
		{name: "owner helper", agent: func(a api.Agent) api.Agent { a.Role = api.AgentRoleOwnerHelper; return a }},
		{name: "superseded run", agent: func(a api.Agent) api.Agent { a.RunID = "run_00000000000000a2"; return a }},
		{name: "closed", agent: func(a api.Agent) api.Agent { a.Status = api.AgentClosed; return a }},
		{name: "exited", agent: func(a api.Agent) api.Agent { a.Status = api.AgentExited; return a }},
		{name: "retired", agent: func(a api.Agent) api.Agent { a.Status = api.AgentRetired; return a }},
		{name: "claude runtime", agent: func(a api.Agent) api.Agent { a.Runtime = "claude"; return a }},
		{name: "hub refuses", agent: func(a api.Agent) api.Agent { a.ID = "agt_00000000000000ff"; return a }},
		{name: "expired", agent: func(a api.Agent) api.Agent { return a }, age: pendingBindingTTL + time.Minute},
	}
	for _, tc := range cases {
		h.up.Store(false)
		autoBindRuntime(e, "status")
		if _, err := os.Stat(pendingPath); err != nil {
			t.Fatalf("%s: pending binding not saved: %v", tc.name, err)
		}
		a := tc.agent(base)
		h.agent.Store(&a)
		h.up.Store(true)
		retryPendingBindings(context.Background(), relayDir(), time.Now().Add(tc.age), relayTestClient)
		if b, ok := readBinding(t, e.hub, e.agent); ok {
			t.Fatalf("%s: binding written: %+v", tc.name, b)
		}
		if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
			t.Fatalf("%s: pending record kept: %v", tc.name, err)
		}
	}
	h.agent.Store(&base)

	// A registered owner helper: its binding is the registration's alone.
	helper := runtimeBinding{Hub: e.hub, Task: e.task, Agent: e.agent, Run: e.runID, Thread: thread, Codex: "/usr/local/bin/codex", Runtime: "codex", Session: "owner", Role: api.AgentRoleOwnerHelper, CreatedAt: time.Now().UTC()}
	helperPath := filepath.Join(relayDir(), bindingKey(helper)+".binding.json")
	if err := writePrivateJSON(helperPath, helper); err != nil {
		t.Fatal(err)
	}
	h.up.Store(false)
	t.Setenv("CODEX_THREAD_ID", "00000000-0000-4000-8000-0000000000a9")
	autoBindRuntime(e, "inbox")
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("owner helper saved a pending record: %v", err)
	}
	// A pending record that reaches the relay anyway never replaces it.
	stray := helper
	stray.Role, stray.Session, stray.Thread = "", "tt-startup-bind", "00000000-0000-4000-8000-0000000000a9"
	if err := savePendingBinding(stray); err != nil {
		t.Fatal(err)
	}
	h.up.Store(true)
	retryPendingBindings(context.Background(), relayDir(), time.Now(), relayTestClient)
	if kept, _ := readBinding(t, e.hub, e.agent); kept != helper {
		t.Fatalf("helper registration changed: %+v", kept)
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending record kept beside a helper registration: %v", err)
	}
}

// Review #13301 f7 (wi_35b27816cdfeda15): an unsafe broker skip returns its
// own obligation to the inbox path. While that fallback keeps failing, a later
// obligation keeps its own broker job, so it is woken once, not also by the
// inbox. The relay loop reaches the inbox before the later job's lease when
// the broker check is spaced or backed off.
func TestRelayClaudeStuckInboxFallbackWakesLaterObligationOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Stuck fallback"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "test", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "test", Session: "claude", Runtime: "claude"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: worker.ID, RunID: worker.RunID}, by); err != nil {
		t.Fatal(err)
	}
	review := func(subject string) api.Message {
		t.Helper()
		m, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: worker.ID, Envelope: &api.Envelope{Kind: "review", To: "worker", Subject: subject, Body: api.EnvelopeBody{Candidate: "abc1234", Scope: "fixture", Acceptance: map[string]string{"a1": "check"}}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := review("Review the first fixture")
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	defer srv.Close()
	client, err := api.NewClient(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: worker.ID, Run: worker.RunID, Thread: "00000000-0000-4000-8000-000000000001", Runtime: "claude", Session: "claude"}
	p := relayProgress{Run: b.Run, Thread: b.Thread}
	var prompts []string
	stuck := func(_ context.Context, _ runtimeBinding, prompt string) error {
		prompts = append(prompts, prompt)
		return fmt.Errorf("%w: turn in progress", errClaudeWakeUnsafe)
	}
	idle := func(_ context.Context, _ runtimeBinding, prompt string) error {
		prompts = append(prompts, prompt)
		return nil
	}
	wakesFor := func(seq int64) int {
		n := 0
		for _, prompt := range prompts {
			for _, match := range brokerPromptSeq.FindAllString(prompt, -1) {
				if match == fmt.Sprintf("#%d", seq) {
					n++
				}
			}
		}
		return n
	}
	t0 := time.Now().Add(time.Second)
	if handled, err := relayWakeJob(ctx, b, &p, client, t0, stuck); !handled || err == nil || !p.ClaudePendingInbox || !slices.Equal(p.ClaudePendingSeqs, []int64{first.Seq}) {
		t.Fatalf("unsafe broker skip: handled=%v err=%v progress=%+v", handled, err, p)
	}
	// The fallback keeps failing on later idle-spaced passes.
	for i, at := range []time.Time{t0.Add(20 * time.Second), t0.Add(40 * time.Second)} {
		if err := relayOne(ctx, b, &p, client, at, stuck); err == nil || !p.ClaudePendingInbox {
			t.Fatalf("stuck fallback %d: err=%v progress=%+v", i, err, p)
		}
	}
	if wakesFor(first.Seq) != 3 {
		t.Fatalf("fallback attempts for the skipped obligation = %d, want 3: %q", wakesFor(first.Seq), prompts)
	}
	data, err := json.Marshal(p)
	if err != nil || json.Unmarshal(data, &p) != nil || !slices.Equal(p.ClaudePendingSeqs, []int64{first.Seq}) {
		t.Fatalf("pending sequences lost on reload: %v %s", err, data)
	}
	later := review("Review the later fixture")
	prompts = nil
	// The pane goes idle. The inbox pass runs before the later job's lease.
	tIdle := t0.Add(60 * time.Second)
	if err := relayOne(ctx, b, &p, client, tIdle, idle); err != nil || len(prompts) != 1 || wakesFor(first.Seq) != 1 || p.ClaudePendingInbox || p.ClaudePendingSeqs != nil {
		t.Fatalf("inbox fallback: prompts=%q progress=%+v err=%v", prompts, p, err)
	}
	if handled, err := relayWakeJob(ctx, b, &p, client, tIdle, idle); !handled || err != nil {
		t.Fatalf("later broker job: handled=%v err=%v prompts=%q", handled, err, prompts)
	}
	if err := relayOne(ctx, b, &p, client, tIdle.Add(20*time.Second), idle); err != nil {
		t.Fatal(err)
	}
	if n := wakesFor(later.Seq); n != 1 || len(prompts) != 2 || !strings.Contains(prompts[1], " Wake wake_") {
		t.Fatalf("later obligation woken %d times, want once by its broker job: %q", n, prompts)
	}
}

// A progress record saved before ClaudePendingSeqs existed has a pending
// fallback and no sequences; its fallback still passes every message, so the
// relay upgrade cannot lose the skipped obligation's wake.
func TestRelayClaudeLegacyPendingFallbackPassesEveryMessage(t *testing.T) {
	msgs := []api.Message{{Seq: 7, To: "agt_self", Envelope: &api.Envelope{Kind: api.EnvelopeKindReview}}}
	p := relayProgress{BrokerWakes: true, ClaudePendingInbox: true}
	got, err := relayInboxEligible(context.Background(), nil, runtimeBinding{Agent: "agt_self"}, &p, msgs)
	if err != nil || len(got) != 1 {
		t.Fatalf("legacy fallback = %+v, %v", got, err)
	}
	p.ClaudePendingSeqs = []int64{6}
	if got, err = relayInboxEligible(context.Background(), nil, runtimeBinding{Agent: "agt_self"}, &p, msgs); err != nil || len(got) != 0 {
		t.Fatalf("narrowed fallback passed another obligation: %+v, %v", got, err)
	}
	seqs := []int64(nil)
	for seq := int64(1); seq <= claudePendingSeqMax+3; seq++ {
		seqs = addClaudePendingSeq(addClaudePendingSeq(seqs, seq), seq)
	}
	if len(seqs) != claudePendingSeqMax || seqs[0] != 4 || seqs[len(seqs)-1] != claudePendingSeqMax+3 {
		t.Fatalf("bounded pending sequences = %v", seqs)
	}
}

// wi_a10e9d0a1c435daf (k3 of wi_35b27816cdfeda15): a Claude broker prompt for
// a message the agent has already read names tt inbox --seq, because tt inbox
// --unread no longer prints it. An unread message and the owner helper, whose
// tt helper inbox has no --seq form, keep the short prompt.
func TestClaudeBrokerPromptFullTextPointerOnlyForReadMessages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Broker pointer"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "test", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "test", Session: "claude", Runtime: "claude"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: worker.ID, RunID: worker.RunID}, by); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	defer srv.Close()
	client, err := api.NewClient(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: worker.ID, Run: worker.RunID, Thread: "00000000-0000-4000-8000-000000000001", Runtime: "claude", Session: "claude"}
	p := relayProgress{Run: b.Run, Thread: b.Thread}
	var prompts []string
	idle := func(_ context.Context, _ runtimeBinding, prompt string) error {
		prompts = append(prompts, prompt)
		return nil
	}
	wake := func(b runtimeBinding, at time.Time, read bool) (api.Message, string) {
		t.Helper()
		m, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: worker.ID, Envelope: &api.Envelope{Kind: "review", To: "worker", Subject: "Review the pointer fixture", Body: api.EnvelopeBody{Candidate: "abc1234", Scope: "fixture", Acceptance: map[string]string{"a1": "check"}}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		if read {
			if err := st.MarkRead(ctx, task.ID, api.MarkReadRequest{AgentID: worker.ID, UpTo: m.Seq}); err != nil {
				t.Fatal(err)
			}
		}
		prompts = nil
		if handled, err := relayWakeJob(ctx, b, &p, client, at, idle); !handled || err != nil || len(prompts) != 1 {
			t.Fatalf("broker wake for #%d: handled=%v err=%v prompts=%q", m.Seq, handled, err, prompts)
		}
		return m, prompts[0]
	}
	t0 := time.Now().Add(time.Second)
	if _, prompt := wake(b, t0, false); strings.Contains(prompt, "--seq") || !strings.Contains(prompt, "Run tt inbox --unread --mark-read. Wake wake_") {
		t.Fatalf("unread message prompt %q", prompt)
	}
	m, prompt := wake(b, t0.Add(20*time.Second), true)
	if want := fmt.Sprintf("Run tt inbox --unread --mark-read. Full text: tt inbox --seq %d. Wake wake_", m.Seq); !strings.Contains(prompt, want) || strings.Count(prompt, "--seq") != 1 {
		t.Fatalf("read message prompt %q lacks %q", prompt, want)
	}
	// The check before Enter still recognizes the longer prompt as the
	// editor wraps it, including the padded rows of a real 80-column pane.
	for _, width := range []int{80, 40} {
		for _, pad := range []bool{false, true} {
			var lines []string
			line := "❯ "
			for _, word := range strings.Fields(prompt) {
				if len([]rune(line))+len([]rune(word))+1 > width && line != "❯ " {
					if pad {
						line += strings.Repeat(" ", width-len([]rune(line)))
					}
					lines = append(lines, line)
					line = "  " + word
				} else {
					if line != "❯ " && line != "  " {
						line += " "
					}
					line += word
				}
			}
			lines = append(lines, line, strings.Repeat("─", width), "  ⏵⏵ bypass permissions on (shift+tab to cycle)")
			if len(lines) < 4 || !exactClaudeInput(strings.Join(lines, "\n"), prompt) {
				t.Fatalf("wrapped %d-column input (padded %v) not recognized: %q", width, pad, lines)
			}
		}
	}
	helper := b
	helper.Role = api.AgentRoleOwnerHelper
	if _, prompt := wake(helper, t0.Add(40*time.Second), true); strings.Contains(prompt, "--seq") || strings.Contains(prompt, "tt inbox") || !strings.Contains(prompt, "Run tt helper inbox --task "+task.ID+". Wake wake_") {
		t.Fatalf("helper prompt %q", prompt)
	}
	if got := claudeBrokerFullText("", 7); got != "" {
		t.Fatalf("empty prompt became %q", got)
	}
}

// An owner close (TailOS or tt close) is seen on this host only by the relay's
// helper heartbeat (bug wi_b134e2c694f51b54): for the binding's exact run shown
// closed or exited, it marks the helper file once and posts no heartbeat, so
// the session's later calls are not ledgered. An open run and a file naming a
// newer run are left as they are.
func TestRelayHelperHeartbeatMarksOwnerClosedRun(t *testing.T) {
	setToolLedger(t, &toolLedgerDeadline, 30*time.Second) // the identity is the subject here, not timing
	var mu sync.Mutex
	var current api.Agent
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events") {
			posts++
			json.NewEncoder(w).Encode(api.Event{})
			return
		}
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/agents/"+helperLedgerAgent) {
			t.Errorf("unexpected hub call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(current)
	}))
	defer server.Close()
	c, err := api.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	alive := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	setHub := func(run, status string) {
		mu.Lock()
		defer mu.Unlock()
		current, posts = api.Agent{ID: helperLedgerAgent, RunID: run, Role: api.AgentRoleOwnerHelper, Status: status, Online: status == api.AgentRunning}, 0
	}
	hubPosts := func() int {
		mu.Lock()
		defer mu.Unlock()
		return posts
	}
	// setup gives an isolated host with the helper file and its relay binding
	// for helperLedgerRun, and the helper's Claude session.
	setup := func(t *testing.T) (root string, file ownerHelperFile, b runtimeBinding, e env) {
		root, _ = helperLedgerSandbox(t)
		file = helperLedgerFile()
		file.Hub = server.URL
		writeHelperLedgerFile(t, file)
		t.Setenv("CLAUDE_CODE_SESSION_ID", file.Thread)
		b = runtimeBinding{Hub: server.URL, Task: file.Task, Agent: file.Agent, Run: file.Run, Thread: file.Thread, Runtime: "claude", Role: api.AgentRoleOwnerHelper}
		return root, file, b, helperLedgerEnv(t)
	}
	call := func(id string) map[string]any {
		return map[string]any{"session_id": helperLedgerThread, "tool_use_id": id, "tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}, "duration_ms": 1}
	}
	hookCall := func(t *testing.T, e env, id string) {
		t.Helper()
		toolHook(t, e, "PreToolUse", call(id))
		toolHook(t, e, "PostToolUse", call(id))
	}
	now := time.Now()

	for _, status := range []string{api.AgentClosed, api.AgentExited} {
		t.Run(status, func(t *testing.T) {
			root, file, b, e := setup(t)
			hookCall(t, e, "u1")
			if rows := toolLedgerRows(t, root, file.Agent); len(rows) != 1 {
				t.Fatalf("open helper rows = %v", rows)
			}
			setHub(file.Run, status)
			var p relayProgress
			if err := relayHelperHeartbeat(context.Background(), b, &p, c, now, alive); err != nil {
				t.Fatal(err)
			}
			want := file
			want.ClosedRun = file.Run
			if state, err := loadOwnerHelperFile(file.Hub, file.Task); err != nil || state != want {
				t.Fatalf("helper file %+v %v; want %+v", state, err, want)
			}
			if n := hubPosts(); n != 0 {
				t.Fatalf("%s run posted %d heartbeat(s)", status, n)
			}
			if _, ok := offlineOwnerHelper(); ok {
				t.Fatal("the offline lookup still matches the closed run")
			}
			hookCall(t, e, "u2")
			if rows := toolLedgerRows(t, root, file.Agent); len(rows) != 1 {
				t.Fatalf("a call after the close was ledgered: %v", rows)
			}
			// A later pass leaves the marked file as it is.
			path := ownerHelperPath(file.Hub, file.Task)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := relayHelperHeartbeat(context.Background(), b, &p, c, now.Add(31*time.Second), alive); err != nil {
				t.Fatal(err)
			}
			if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("a second pass rewrote the marked file: %v", err)
			}
			if n := hubPosts(); n != 0 {
				t.Fatalf("second pass posted %d heartbeat(s)", n)
			}
		})
	}

	t.Run("mark error", func(t *testing.T) {
		_, file, b, _ := setup(t)
		dir := filepath.Dir(ownerHelperPath(file.Hub, file.Task))
		if err := os.Chmod(dir, 0500); err != nil { // the host lock cannot be created
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0700) })
		setHub(file.Run, api.AgentClosed)
		var p relayProgress
		if err := relayHelperHeartbeat(context.Background(), b, &p, c, now, alive); err == nil {
			t.Fatal("a failed mark returned no error")
		}
		if n := hubPosts(); n != 0 {
			t.Fatalf("a failed mark posted %d heartbeat(s)", n)
		}
		if state, err := loadOwnerHelperFile(file.Hub, file.Task); err != nil || state != file {
			t.Fatalf("helper file %+v %v", state, err)
		}
	})

	t.Run("open", func(t *testing.T) {
		root, file, b, e := setup(t)
		path := ownerHelperPath(file.Hub, file.Task)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		setHub(file.Run, api.AgentRunning)
		var p relayProgress
		if err := relayHelperHeartbeat(context.Background(), b, &p, c, now, alive); err != nil {
			t.Fatal(err)
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("open run changed the helper file: %s %v", after, err)
		}
		if n := hubPosts(); n != 1 {
			t.Fatalf("open run posted %d heartbeat(s); want 1", n)
		}
		hookCall(t, e, "u1")
		if rows := toolLedgerRows(t, root, file.Agent); len(rows) != 1 || rows[0]["run"] != file.Run {
			t.Fatalf("open helper rows = %v", rows)
		}
	})

	t.Run("re-registered", func(t *testing.T) {
		// The old run's binding is still read while the file already names
		// the run a later registration returned.
		root, file, b, e := setup(t)
		newer := file
		newer.Run = "run_helper_newer"
		writeHelperLedgerFile(t, newer)
		setHub(file.Run, api.AgentClosed)
		var p relayProgress
		if err := relayHelperHeartbeat(context.Background(), b, &p, c, now, alive); err != nil {
			t.Fatal(err)
		}
		if state, err := loadOwnerHelperFile(newer.Hub, newer.Task); err != nil || state != newer {
			t.Fatalf("the newer registration was marked: %+v %v", state, err)
		}
		if n := hubPosts(); n != 0 {
			t.Fatalf("closed run posted %d heartbeat(s)", n)
		}
		hookCall(t, e, "u1")
		if rows := toolLedgerRows(t, root, newer.Agent); len(rows) != 1 || rows[0]["run"] != newer.Run {
			t.Fatalf("re-registered helper rows = %v; want one under %s", rows, newer.Run)
		}
	})
}

// relayTestClock stands in for relayPassClock, so a test can make a pass or
// a team queue tick take minutes without waiting for them.
type relayTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newRelayTestClock(t *testing.T) *relayTestClock {
	t.Helper()
	c := &relayTestClock{now: time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)}
	old := relayPassClock
	t.Cleanup(func() { relayPassClock = old })
	relayPassClock = c.Now
	return c
}

func (c *relayTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *relayTestClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func stubRelayTeamQueue(t *testing.T, tick func(context.Context) error) {
	t.Helper()
	old := relayTeamQueue
	t.Cleanup(func() { relayTeamQueue = old })
	relayTeamQueue = tick
}

// A git that does not answer is killed at activityWorktreeTimeout, its error
// is cached for the pass, and the activity cursor keeps the last signature
// (wi_44e5b080b7e7d846 r2).
func TestActivityWorktreeGitTimeoutKeepsPreviousSignature(t *testing.T) {
	old := activityWorktreeTimeout
	t.Cleanup(func() { activityWorktreeTimeout = old })
	// Long enough for a loaded host to start the stub shell.
	activityWorktreeTimeout = time.Second
	writeGit := func(t *testing.T, dir, calls, body string) {
		t.Helper()
		content := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + calls + "'\n" + body
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, body string
		calls      int
	}{
		// The shell's sleep keeps stdout open after the shell is killed, so
		// only WaitDelay ends the wait inside 3s.
		{"status", "sleep 5\n", 1},
		{"rev-parse", "if [ \"$4\" = 'rev-parse' ]; then exec sleep 30; fi\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			calls := filepath.Join(home, "calls")
			writeGit(t, home, calls, tc.body)
			t.Setenv("PATH", home+":"+os.Getenv("PATH"))
			ctx := context.WithValue(context.Background(), activityWorktreeContextKey{}, activityWorktreeCache{})
			began := time.Now()
			for i := 0; i < 2; i++ {
				if signature, err := cachedActivityWorktree(ctx, home); err == nil || signature != "" {
					t.Fatalf("probe %d of a git that does not answer = %q, %v; want an error", i, signature, err)
				}
			}
			if elapsed := time.Since(began); elapsed > 3*time.Second {
				t.Fatalf("two probes of a git that does not answer took %s, want under 3s", elapsed)
			}
			data, _ := os.ReadFile(calls)
			if got := strings.Count(string(data), "\n"); got != tc.calls {
				t.Fatalf("git calls in one pass = %d, want %d: %q", got, tc.calls, data)
			}
		})
	}

	t.Run("cursor", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CODEX_HOME", home)
		t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
		calls := filepath.Join(home, "calls")
		writeGit(t, home, calls, "exec sleep 30\n")
		t.Setenv("PATH", home+":"+os.Getenv("PATH"))
		now := time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)
		changedAt := now.Add(-time.Hour)
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				_ = json.NewEncoder(w).Encode(api.Agent{RunID: "run_0123456789abcdef", Status: api.AgentRunning, Online: true, Session: "fake"})
				return
			}
			w.WriteHeader(http.StatusConflict)
		}))
		defer hub.Close()
		client, _ := api.NewClient(hub.URL, time.Second)
		b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: "12345678-1234-1234-1234-123456789abc", Runtime: "codex", Cwd: home, CreatedAt: now}
		path := filepath.Join(relayDir(), bindingKey(b)+".activity.json")
		if err := os.MkdirAll(relayDir(), 0700); err != nil {
			t.Fatal(err)
		}
		seeded := relayActivityState{activityCursor: activityCursor{Run: b.Run, Thread: b.Thread, Worktree: "prev", WorktreeChangedAt: changedAt}, RequestNonce: "nonce"}
		if err := writePrivateJSON(path, seeded); err != nil {
			t.Fatal(err)
		}
		live := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
		tick := func(at time.Time) activityCursor {
			t.Helper()
			ctx := context.WithValue(context.Background(), activityWorktreeContextKey{}, activityWorktreeCache{})
			if err := relayActivityTick(ctx, b, client, at, live); err != nil {
				t.Fatal(err)
			}
			var saved relayActivityState
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			return saved.activityCursor
		}
		if c := tick(now); c.Worktree != "prev" || !c.WorktreeChangedAt.Equal(changedAt) {
			t.Fatalf("a timed-out git replaced the cursor's worktree: %q changed %s", c.Worktree, c.WorktreeChangedAt)
		}
		if data, _ := os.ReadFile(calls); !strings.Contains(string(data), " status ") {
			t.Fatalf("the activity tick never probed the worktree: %q", data)
		}
		// The same tick with a git that answers records the change, so the
		// timed-out tick above reached the worktree probe.
		writeGit(t, home, calls, "if [ \"$4\" = 'rev-parse' ]; then echo head; fi\n")
		later := now.Add(16 * time.Second)
		if c := tick(later); c.Worktree == "prev" || c.Worktree == "" || !c.WorktreeChangedAt.Equal(later) {
			t.Fatalf("a git that answers did not record the change: %q changed %s", c.Worktree, c.WorktreeChangedAt)
		}
	})
}

// A pass over 30s prints one line with its phase times, slowest step and
// slowest hub call, at most once per two minutes, and no private text.
func TestRelaySlowPassLogsPhaseTimes(t *testing.T) {
	clock := newRelayTestClock(t)
	stubRelayTeamQueue(t, func(context.Context) error { return nil })
	steward := func(context.Context) error { return nil }
	handler := func(context.Context) error { return nil }
	// agentDelay is how long each read of the bound agent takes on the clock.
	var agentDelay atomic.Int64
	var passes int
	pause := func() bool {
		passes++
		switch passes {
		case 1:
			// The second pass is slow too, inside the two-minute limit.
		case 2:
			// The third pass is slow after the limit.
			clock.advance(2 * time.Minute)
		case 3:
			agentDelay.Store(0)
		default:
			return false
		}
		return true
	}
	f := newRotationLoopFixture(t, handler, steward, pause)
	var agentPath string
	agentDelay.Store(int64(35 * time.Second))
	hook := func(r *http.Request) {
		if r.Method == "GET" && r.URL.Path == agentPath {
			clock.advance(time.Duration(agentDelay.Load()))
		}
	}
	f.onAgentRead.Store(&hook)
	data, err := os.ReadFile(func() string { paths, _ := filepath.Glob(filepath.Join(relayDir(), "*.binding.json")); return paths[0] }())
	if err != nil {
		t.Fatal(err)
	}
	var b runtimeBinding
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	agentPath = "/v1/tasks/" + b.Task + "/agents/" + b.Agent
	oldCleanup := relayLoopCleanup
	t.Cleanup(func() { relayLoopCleanup = oldCleanup })
	var cleanups atomic.Int32
	relayLoopCleanup = func() {
		// Only the first pass's cleanup is slow: later ones may wait for a
		// queue pass still in flight.
		if cleanups.Add(1) == 1 {
			clock.advance(20 * time.Second)
		}
	}
	stderr, err := captureRelayOutput(t, true, func() error { return cmdRelay(nil) })
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "] ") && strings.Contains(line, " slow pass ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("slow pass lines over three slow passes and one fast one = %d, want 2:\n%s", len(lines), stderr)
	}
	first := regexp.MustCompile(`^\[tt relay\] \S+ slow pass (\d+\.\d)s: start 20\.0s, bindings (\d+\.\d)s \(1 bindings\), end 0\.0s; slowest step (.+) (\d+\.\d)s; slowest hub call GET ` + regexp.QuoteMeta(agentPath) + ` 35\.0s$`)
	if m := first.FindStringSubmatch(lines[0]); m == nil || m[1] == "20.0" {
		t.Fatalf("first slow pass line = %q, want start, bindings and end times, the slowest step and hub call", lines[0])
	}
	if !strings.HasSuffix(lines[1], "; 1 slow pass not logged") || strings.Contains(lines[1], "start 20.0s") {
		t.Fatalf("slow pass line after the limit = %q, want the held-back pass counted", lines[1])
	}
	for _, line := range lines {
		if strings.Contains(line, rotationLoopSentinel) || strings.Contains(line, "?") {
			t.Fatalf("slow pass line carries a query or private text: %q", line)
		}
	}
}

// The team queue tick runs in its own pass: while one blocks, the loop keeps
// visiting the bound agent, starts no second tick, and runs no cleanup.
func TestRelayLoopWakesWhileTeamQueueTickBlocks(t *testing.T) {
	clock := newRelayTestClock(t)
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var ticks atomic.Int32
	stubRelayTeamQueue(t, func(ctx context.Context) error {
		if ticks.Add(1) != 1 {
			return nil
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		// The tick took two minutes on the relay's clock.
		clock.advance(2 * time.Minute)
		close(returned)
		return nil
	})
	var cleanups atomic.Int32
	oldCleanup := relayLoopCleanup
	t.Cleanup(func() { relayLoopCleanup = oldCleanup })
	relayLoopCleanup = func() { cleanups.Add(1) }
	steward := func(context.Context) error { return nil }
	handler := func(context.Context) error { return nil }
	var f *rotationLoopFixture
	var mu sync.Mutex
	var visits []time.Time
	var passes int
	var readsWhenBlocked, readsBeforeRelease, ticksBeforeRelease, cleanupsBeforeRelease int32
	var visitsBeforeRelease int
	var deadline time.Time
	pause := func() bool {
		passes++
		switch {
		case passes == 1:
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				t.Error("team queue tick never started")
				return false
			}
			readsWhenBlocked = f.agentReads.Load()
			return true
		case passes < 5:
			// Each binding visit takes ten seconds on the relay's clock.
			return true
		case passes == 5:
			readsBeforeRelease, ticksBeforeRelease, cleanupsBeforeRelease = f.agentReads.Load(), ticks.Load(), cleanups.Load()
			mu.Lock()
			visitsBeforeRelease = len(visits)
			mu.Unlock()
			unblock()
			<-returned
			deadline = time.Now().Add(15 * time.Second)
			return true
		}
		// The next pass to collect the tick cleans up again.
		if cleanups.Load() > cleanupsBeforeRelease {
			return false
		}
		if time.Now().After(deadline) {
			t.Error("the team queue pass was never collected after it returned")
			return false
		}
		time.Sleep(10 * time.Millisecond)
		return true
	}
	f = newRotationLoopFixture(t, handler, steward, pause)
	hook := func(r *http.Request) {
		if r.Method == "GET" && strings.Count(r.URL.Path, "/") == 5 {
			clock.advance(10 * time.Second)
			mu.Lock()
			visits = append(visits, clock.Now())
			mu.Unlock()
		}
	}
	f.onAgentRead.Store(&hook)
	stderr, err := captureRelayOutput(t, true, func() error { return cmdRelay(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if readsBeforeRelease <= readsWhenBlocked {
		t.Fatalf("no pass read the agent while the team queue tick blocked: %d then %d", readsWhenBlocked, readsBeforeRelease)
	}
	if ticksBeforeRelease != 1 {
		t.Fatalf("team queue ticks while one blocked = %d, want 1", ticksBeforeRelease)
	}
	// Only the first pass, before the tick started, cleaned up.
	if cleanupsBeforeRelease != 1 {
		t.Fatalf("loop cleanups with a team queue pass in flight = %d, want only the first pass's", cleanupsBeforeRelease)
	}
	mu.Lock()
	visits = visits[:visitsBeforeRelease]
	for i := 1; i < len(visits); i++ {
		if gap := visits[i].Sub(visits[i-1]); gap >= time.Minute {
			t.Fatalf("binding visits %d and %d were %s apart on the relay's clock, want under 60s", i-1, i, gap)
		}
	}
	if len(visits) < 4 {
		t.Fatalf("binding visits = %d, want one per pass", len(visits))
	}
	mu.Unlock()
	if strings.Contains(stderr, "slowest step team queue") {
		t.Fatalf("a pass waited for the team queue tick:\n%s", stderr)
	}
	// The tick's own two minutes plus the visits that ran beside it.
	m := regexp.MustCompile(`\] \S+ slow team queue pass (\d+)\.\ds\n`).FindStringSubmatch(stderr)
	if took, _ := strconv.Atoi(func() string {
		if m == nil {
			return ""
		}
		return m[1]
	}()); took < 120 {
		t.Fatalf("no slow team queue pass line of at least 120s:\n%s", stderr)
	}
}

func TestRelayOnceWaitsForTeamQueuePassBeforeWaking(t *testing.T) {
	var f *rotationLoopFixture
	var readsAtReturn atomic.Int32
	readsAtReturn.Store(-1)
	returned := make(chan struct{})
	// The tick holds for half a second, or ends early once a wake pass reads
	// the bound agent, as it would if once mode did not wait.
	stubRelayTeamQueue(t, func(ctx context.Context) error {
		defer close(returned)
		hold := time.After(500 * time.Millisecond)
		for f.agentReads.Load() == 0 {
			select {
			case <-hold:
				readsAtReturn.Store(f.agentReads.Load())
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
		readsAtReturn.Store(f.agentReads.Load())
		return nil
	})
	noop := func(context.Context) error { return nil }
	pause := func() bool { t.Error("once mode paused between passes"); return false }
	f = newRotationLoopFixture(t, noop, noop, pause)
	if err := cmdRelay([]string{"--once"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(15 * time.Second):
		t.Fatal("team queue tick never returned")
	}
	if got := readsAtReturn.Load(); got != 0 {
		t.Fatalf("agent reads before the team queue pass returned = %d, want 0", got)
	}
	if f.agentReads.Load() == 0 {
		t.Fatal("once mode never ran the wake pass for the bound agent")
	}
}

func TestRelayTeamQueuePassObservesBackoff(t *testing.T) {
	rateLimited := &api.HTTPError{Status: http.StatusTooManyRequests, Msg: "synthetic 429"}
	var tickErr error
	var ticks int
	var hold chan struct{}
	stubRelayTeamQueue(t, func(context.Context) error {
		ticks++
		if hold != nil {
			<-hold
		}
		return tickErr
	})
	var backoff teamQueuePollBackoff
	var queue relayQueuePass
	// run mirrors the loop: a pass starts only when the backoff is ready.
	run := func() (int, string) {
		t.Helper()
		ticks = 0
		stderr, _ := captureRelayOutput(t, true, func() error {
			if backoff.ready(time.Now()) {
				queue.start()
			}
			queue.collect(&backoff, true)
			return nil
		})
		return ticks, stderr
	}
	tickErr = rateLimited
	if got, stderr := run(); got != 1 || backoff.delay != 6*time.Second || backoff.ready(time.Now()) || !strings.Contains(stderr, "[tt relay] team queue: ") {
		t.Fatalf("queue 429: ticks=%d backoff=%+v stderr=%q", got, backoff, stderr)
	}
	if got, _ := run(); got != 0 {
		t.Fatalf("team queue ran inside the backoff: %d", got)
	}
	backoff.next = time.Now().Add(-time.Second)
	tickErr = errors.New("queue boom")
	if got, stderr := run(); got != 1 || backoff.delay != 6*time.Second || !backoff.ready(time.Now()) || stderr != "[tt relay] team queue: queue boom\n" {
		t.Fatalf("other error: ticks=%d backoff=%+v stderr=%q", got, backoff, stderr)
	}
	tickErr = nil
	if got, stderr := run(); got != 1 || backoff.delay != 0 || stderr != "" {
		t.Fatalf("success did not clear the backoff: ticks=%d backoff=%+v stderr=%q", got, backoff, stderr)
	}
	// A clean tick must not clear spacing that a rotation 429 set while it ran.
	hold = make(chan struct{})
	queue.start()
	backoff.observe(time.Now(), rateLimited)
	close(hold)
	queue.collect(&backoff, true)
	if backoff.delay != 6*time.Second || backoff.ready(time.Now()) {
		t.Fatalf("a clean tick cleared spacing a later 429 set: %+v", backoff)
	}

	// In the loop, a 429 from the queue pass holds the next queue and
	// rotation passes for the backoff spacing.
	tickErr, hold = rateLimited, nil
	var queueTicks, handlerCalls atomic.Int32
	stubRelayTeamQueue(t, func(context.Context) error { queueTicks.Add(1); return rateLimited })
	handler := func(context.Context) error { handlerCalls.Add(1); return nil }
	steward := func(context.Context) error { return nil }
	var passes int
	pause := func() bool {
		passes++
		time.Sleep(50 * time.Millisecond)
		return passes < 12
	}
	newRotationLoopFixture(t, handler, steward, pause)
	stderr, err := captureRelayOutput(t, true, func() error { return cmdRelay(nil) })
	if err != nil {
		t.Fatal(err)
	}
	// Twelve passes fit well inside the first six-second spacing; the first
	// pass's rotation started before the 429 came back.
	if queueTicks.Load() != 1 || handlerCalls.Load() != 1 {
		t.Fatalf("queue ticks = %d, handler ticks = %d over %d passes, want 1 and 1", queueTicks.Load(), handlerCalls.Load(), passes)
	}
	if got := strings.Count(stderr, "[tt relay] team queue: "); got != 1 {
		t.Fatalf("team queue log entries = %d: %s", got, stderr)
	}
}

// relayVisitFixture runs the relay loop against a local hub with many bound
// Codex agents, away from the live hub, relay state, config and tmux socket.
// It counts each agent's visits through the relayVisitRun seam.
type relayVisitFixture struct {
	dir      string
	bindings []runtimeBinding
	// onAgentRead, when set, runs on each read of a bound agent before the
	// hub answers it.
	onAgentRead atomic.Pointer[func(agent string)]
	// onVisit, when set, runs inside each visit before the real one.
	onVisit atomic.Pointer[func(agent string)]
	// onEnd, when set, runs inside each visit after the real one has saved
	// its progress and been counted ended, before the pool frees the binding.
	onEnd atomic.Pointer[func(agent string)]

	mu                      sync.Mutex
	started, ended, running map[string]int
	concurrent              int
	maxConcurrent, maxAgent int
}

func newRelayVisitFixture(t *testing.T, n, workers int, wait time.Duration, pause func() bool) *relayVisitFixture {
	t.Helper()
	f := &relayVisitFixture{started: map[string]int{}, ended: map[string]int{}, running: map[string]int{}}
	f.dir = t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", f.dir)
	t.Setenv("TT_TMUX_SOCKET", "relay-visit-test-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	for _, name := range []string{"TAILTERM_TOKEN", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN"} {
		t.Setenv(name, "")
	}
	runs := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pause"):
			_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{State: api.ProjectPauseActive})
		case strings.HasSuffix(r.URL.Path, "/wake-jobs/lease"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/agents/") && strings.Count(r.URL.Path, "/") == 5:
			agent := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			run, ok := runs[agent]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if hook := f.onAgentRead.Load(); hook != nil {
				(*hook)(agent)
			}
			_ = json.NewEncoder(w).Encode(api.Agent{ID: agent, RunID: run, Name: agent, Status: api.AgentDone, Online: true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("TAILTERM_HUB", server.URL)
	for i := 1; i <= n; i++ {
		b := runtimeBinding{Hub: server.URL, Task: "tsk_0000000000000001", Agent: fmt.Sprintf("agt_%016x", i), Run: fmt.Sprintf("run_%016x", i), Thread: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Codex: filepath.Join(f.dir, "fake-codex")}
		runs[b.Agent] = b.Run
		if err := writePrivateJSON(filepath.Join(f.dir, bindingKey(b)+".binding.json"), b); err != nil {
			t.Fatal(err)
		}
		f.bindings = append(f.bindings, b)
	}
	noop := func(context.Context) error { return nil }
	stubRelayTeamQueue(t, noop)
	oldHandler, oldSteward, oldPause := relayHandlerRotation, relayStewardRotation, relayLoopPause
	oldWorkers, oldWait, oldRun := relayVisitWorkers, relayVisitWait, relayVisitRun
	t.Cleanup(func() {
		relayHandlerRotation, relayStewardRotation, relayLoopPause = oldHandler, oldSteward, oldPause
		relayVisitWorkers, relayVisitWait, relayVisitRun = oldWorkers, oldWait, oldRun
	})
	relayHandlerRotation, relayStewardRotation, relayLoopPause = noop, noop, pause
	relayVisitWorkers, relayVisitWait = workers, wait
	relayVisitRun = func(dir, path, progressPath string, b runtimeBinding, progress relayProgress, pass *relayPassTiming, cache activityWorktreeCache, deps *runtimePromptDeps) {
		f.mu.Lock()
		f.started[b.Agent]++
		f.running[b.Agent]++
		f.concurrent++
		f.maxConcurrent = max(f.maxConcurrent, f.concurrent)
		f.maxAgent = max(f.maxAgent, f.running[b.Agent])
		f.mu.Unlock()
		if hook := f.onVisit.Load(); hook != nil {
			(*hook)(b.Agent)
		}
		relayVisit(dir, path, progressPath, b, progress, pass, cache, deps)
		f.mu.Lock()
		f.running[b.Agent]--
		f.concurrent--
		f.ended[b.Agent]++
		f.mu.Unlock()
		if hook := f.onEnd.Load(); hook != nil {
			(*hook)(b.Agent)
		}
	}
	return f
}

func (f *relayVisitFixture) progressPath(b runtimeBinding) string {
	return filepath.Join(f.dir, bindingKey(b)+".progress.json")
}

// One binding whose hub read blocks no longer holds the others' visits: it
// is detached after relayVisitWait, never visited twice at once, and visited
// again once its visit ends (wi_da5fae2aef5ca94a).
func TestRelayVisitsOtherBindingsWhileOneHubCallBlocks(t *testing.T) {
	var f *relayVisitFixture
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var passes int
	var released bool
	var blockedStarted, blockedEnded int
	var progressWhileBlocked bool
	var otherMin, startedAtRelease int
	var releasedAt time.Time
	deadline := time.Now().Add(15 * time.Second)
	pause := func() bool {
		passes++
		if time.Now().After(deadline) {
			f.mu.Lock()
			t.Errorf("pass %d timed out: started %v, ended %v", passes, f.started, f.ended)
			f.mu.Unlock()
			return false
		}
		blocked := f.bindings[0]
		f.mu.Lock()
		defer f.mu.Unlock()
		if !released {
			select {
			case <-entered:
			default:
				return true
			}
			otherMin = -1
			for _, b := range f.bindings[1:] {
				if otherMin < 0 || f.ended[b.Agent] < otherMin {
					otherMin = f.ended[b.Agent]
				}
			}
			if otherMin < 3 {
				return true
			}
			blockedStarted, blockedEnded = f.started[blocked.Agent], f.ended[blocked.Agent]
			_, err := os.Stat(f.progressPath(blocked))
			progressWhileBlocked = err == nil
			startedAtRelease = f.started[blocked.Agent]
			released = true
			releasedAt = time.Now()
			unblock()
			return true
		}
		// After release: the blocked visit ends and the binding is visited again.
		if f.ended[blocked.Agent] >= 2 {
			return false
		}
		time.Sleep(5 * time.Millisecond)
		return true
	}
	f = newRelayVisitFixture(t, 8, 2, 50*time.Millisecond, pause)
	t.Cleanup(unblock)
	// This test is about the pool, not the cleanup bound: the hold must not
	// stop the other bindings' visits while the blocked one runs.
	oldBound := relayCleanupSkipBound
	t.Cleanup(func() { relayCleanupSkipBound = oldBound })
	relayCleanupSkipBound = 1000
	blockedAgent := f.bindings[0].Agent
	hook := func(agent string) {
		if agent != blockedAgent {
			return
		}
		first := false
		enterOnce.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
	}
	f.onAgentRead.Store(&hook)
	// At the blocked visit's end, before the pool frees its binding for a
	// later visit, the progress file can only hold that visit's save.
	var firstEndMtime time.Time
	endedAtFirstEnd := -1
	var endOnce sync.Once
	endHook := func(agent string) {
		if agent != blockedAgent {
			return
		}
		endOnce.Do(func() {
			f.mu.Lock()
			endedAtFirstEnd = f.ended[agent]
			f.mu.Unlock()
			if info, err := os.Stat(f.progressPath(f.bindings[0])); err == nil {
				firstEndMtime = info.ModTime()
			}
		})
	}
	f.onEnd.Store(&endHook)
	if err := cmdRelay(nil); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("the blocked read was never released")
	}
	if endedAtFirstEnd != 1 {
		t.Fatalf("blocked binding visits ended at its first end = %d, want 1", endedAtFirstEnd)
	}
	if firstEndMtime.IsZero() || firstEndMtime.Before(releasedAt) {
		t.Fatalf("blocked binding progress at its first end saved at %v, want a save from the blocked visit after its release at %v", firstEndMtime, releasedAt)
	}
	if otherMin < 3 {
		t.Fatalf("other bindings visited at least %d times while one blocked, want 3", otherMin)
	}
	if blockedStarted != 1 || blockedEnded != 0 || startedAtRelease != 1 {
		t.Fatalf("blocked binding visits while blocked: started %d, ended %d, want 1 and 0", blockedStarted, blockedEnded)
	}
	if progressWhileBlocked {
		t.Fatal("the blocked binding's progress was saved before its visit ended")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxAgent != 1 {
		t.Fatalf("most visits of one binding at once = %d, want 1", f.maxAgent)
	}
	if f.ended[blockedAgent] < 2 {
		t.Fatalf("blocked binding visits after release = %d ended, want the blocked one and another", f.ended[blockedAgent])
	}
	if _, err := os.Stat(f.progressPath(f.bindings[0])); err != nil {
		t.Fatalf("the blocked visit saved no progress after release: %v", err)
	}
}

// A visit detached past relayVisitWait holds the loop's session cleanup.
// After relayCleanupSkipBound skipped passes in a row the loop starts no new
// visit; cleanup runs once the visit ends, never beside a visit, and logs one
// line for the whole run of skips (wi_1328c6bd412ea61d).
func TestRelayCleanupSkipBoundHoldsVisitsForDetachedVisit(t *testing.T) {
	var f *relayVisitFixture
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// The visit stays blocked this many passes past the bound.
	const pastBound = 4
	var passes, releasedAfter, cleanupsAtRelease int
	var startedAfter []int  // visits started in all, after each pass
	var cleanupPasses []int // the pass each cleanup ran in
	var cleanupBesideVisit bool
	deadline := time.Now().Add(15 * time.Second)
	oldCleanup := relayLoopCleanup
	t.Cleanup(func() { relayLoopCleanup = oldCleanup })
	relayLoopCleanup = func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.concurrent > 0 {
			cleanupBesideVisit = true
		}
		cleanupPasses = append(cleanupPasses, passes+1)
	}
	pause := func() bool {
		passes++
		if time.Now().After(deadline) {
			t.Errorf("pass %d timed out: cleanups in passes %v", passes, cleanupPasses)
			return false
		}
		f.mu.Lock()
		total := 0
		for _, n := range f.started {
			total += n
		}
		startedAfter = append(startedAfter, total)
		cleanups := len(cleanupPasses)
		f.mu.Unlock()
		switch {
		case passes == 1:
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Error("the blocked visit never started")
				return false
			}
		case releasedAfter == 0 && passes == relayCleanupSkipBound+1+pastBound:
			releasedAfter, cleanupsAtRelease = passes, cleanups
			unblock()
		case releasedAfter > 0 && cleanups > cleanupsAtRelease:
			// The pass after cleanup ran again has visited; stop.
			return false
		case releasedAfter > 0:
			time.Sleep(5 * time.Millisecond)
		}
		return true
	}
	f = newRelayVisitFixture(t, 3, 2, 50*time.Millisecond, pause)
	t.Cleanup(unblock)
	blockedAgent := f.bindings[0].Agent
	hook := func(agent string) {
		if agent != blockedAgent {
			return
		}
		first := false
		enterOnce.Do(func() { first = true; close(entered) })
		if first {
			<-release
		}
	}
	f.onAgentRead.Store(&hook)
	stderr, err := captureRelayOutput(t, true, func() error { return cmdRelay(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if releasedAfter == 0 {
		t.Fatalf("the blocked visit was never released after %d passes", passes)
	}
	if cleanupBesideVisit {
		t.Fatal("session cleanup ran while a visit was running")
	}
	if cleanupsAtRelease != 1 || len(cleanupPasses) != 2 || cleanupPasses[0] != 1 {
		t.Fatalf("cleanups ran in passes %v, %d before release, want pass 1 only before release and one after", cleanupPasses, cleanupsAtRelease)
	}
	bound := relayCleanupSkipBound
	if startedAfter[bound-1] <= startedAfter[0] {
		t.Fatalf("visits started after each pass %v, want the other bindings visited before the bound", startedAfter)
	}
	// Passes bound+1 until cleanup ran again started no visit.
	for p := bound + 1; p < cleanupPasses[1]; p++ {
		if startedAfter[p-1] != startedAfter[bound-1] {
			t.Fatalf("visits started after each pass %v, want none started from pass %d until cleanup in pass %d", startedAfter, bound+1, cleanupPasses[1])
		}
	}
	if len(startedAfter) < cleanupPasses[1] || startedAfter[cleanupPasses[1]-1] <= startedAfter[bound-1] {
		t.Fatalf("visits started after each pass %v, want visits again in pass %d once cleanup ran", startedAfter, cleanupPasses[1])
	}
	skipped := cleanupPasses[1] - cleanupPasses[0] - 1
	if skipped <= bound {
		t.Fatalf("cleanup skipped %d passes, want more than the bound %d", skipped, bound)
	}
	line := regexp.MustCompile(`(?m)^\[tt relay\] \S+ session cleanup skipped (\d+) passes$`)
	lines := line.FindAllStringSubmatch(stderr, -1)
	if len(lines) != 1 || lines[0][1] != strconv.Itoa(skipped) {
		t.Fatalf("skip lines = %q, want one counting %d passes:\n%s", lines, skipped, stderr)
	}
}

// The pool runs at most relayVisitWorkers visits at once, one per binding,
// and visits every binding in every pass.
func TestRelayVisitWorkersBoundConcurrency(t *testing.T) {
	const bindings, workers, passTotal = 10, 3, 3
	var f *relayVisitFixture
	var passes int
	var perPass [][]int
	pause := func() bool {
		passes++
		f.mu.Lock()
		var counts []int
		for _, b := range f.bindings {
			counts = append(counts, f.ended[b.Agent])
		}
		f.mu.Unlock()
		perPass = append(perPass, counts)
		return passes < passTotal
	}
	f = newRelayVisitFixture(t, bindings, workers, time.Minute, pause)
	// Each visit holds until the concurrent count reads relayVisitWorkers, so
	// the pool fills; the tail of a pass, with fewer bindings left, goes on
	// after a short wait.
	hook := func(string) {
		hold := time.After(200 * time.Millisecond)
		for {
			f.mu.Lock()
			full := f.concurrent >= workers
			f.mu.Unlock()
			if full {
				return
			}
			select {
			case <-hold:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}
	f.onVisit.Store(&hook)
	if err := cmdRelay(nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxConcurrent != workers {
		t.Fatalf("most visits at once = %d, want relayVisitWorkers %d", f.maxConcurrent, workers)
	}
	if f.maxAgent != 1 {
		t.Fatalf("most visits of one binding at once = %d, want 1", f.maxAgent)
	}
	if len(perPass) != passTotal {
		t.Fatalf("passes = %d, want %d", len(perPass), passTotal)
	}
	for i, counts := range perPass {
		for j, n := range counts {
			if n != i+1 {
				t.Fatalf("binding %d visits by the end of pass %d = %d, want %d (none dropped): %v", j, i+1, n, i+1, perPass)
			}
		}
	}
}

// Under --once a visit past relayVisitWait is not detached: it ends, and its
// progress is saved, before the relay returns.
func TestRelayOnceWaitsForEveryVisit(t *testing.T) {
	pause := func() bool { t.Error("once mode paused between passes"); return false }
	f := newRelayVisitFixture(t, 3, 2, 20*time.Millisecond, pause)
	slow := f.bindings[0].Agent
	hook := func(agent string) {
		if agent == slow {
			time.Sleep(300 * time.Millisecond)
		}
	}
	f.onAgentRead.Store(&hook)
	if err := cmdRelay([]string{"--once"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	ended, started := maps.Clone(f.ended), maps.Clone(f.started)
	f.mu.Unlock()
	for _, b := range f.bindings {
		if started[b.Agent] != 1 || ended[b.Agent] != 1 {
			t.Fatalf("visits of %s when --once returned: started %d, ended %d, want 1 and 1", b.Agent, started[b.Agent], ended[b.Agent])
		}
		if _, err := os.Stat(f.progressPath(b)); err != nil {
			t.Fatalf("progress of %s not saved when --once returned: %v", b.Agent, err)
		}
	}
}
