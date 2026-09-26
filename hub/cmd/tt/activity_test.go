package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

func TestActivityTranscriptMappingAndIncrementalReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	thread := "12345678-1234-1234-1234-123456789abc"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "25")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
	write := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err = f.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	b := runtimeBinding{Runtime: "codex", Thread: thread}
	write("{\"type\":\"task_started\",\"timestamp\":\"2026-09-25T20:00:00Z\"}\n")
	got, err := activityTranscript(b)
	if err != nil || got != path {
		t.Fatalf("mapping %q %v", got, err)
	}
	var c activityCursor
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || !c.SeenTurn || c.Offset == 0 {
		t.Fatalf("first read %+v %v", c, err)
	}
	first := c.Offset
	write("{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":12,\"output_tokens\":3,\"total_tokens\":15}}}}")
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || c.Tokens.Total != 0 || c.Offset <= first {
		t.Fatalf("partial read %+v %v", c, err)
	}
	write("\n")
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || c.Tokens.Total != 15 {
		t.Fatalf("append %+v %v", c, err)
	}
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || c.Tokens.Total != 15 {
		t.Fatalf("replay double count %+v %v", c, err)
	}
	if err := os.WriteFile(path, []byte("{\"type\":\"task_complete\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || !c.TurnComplete || c.Tokens.Total != 15 {
		t.Fatalf("truncate %+v %v", c, err)
	}
	rotated := path + ".old"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}
	write("{\"type\":\"task_started\"}\n")
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || c.TurnComplete {
		t.Fatalf("rotation %+v %v", c, err)
	}
	claudeDir := filepath.Join(home, ".claude", "projects", "-synthetic")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(claudeDir, thread+".jsonl")
	if err := os.WriteFile(claudePath, []byte("{\"type\":\"assistant\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = activityTranscript(runtimeBinding{Runtime: "claude", Thread: thread})
	if err != nil || got != claudePath {
		t.Fatalf("Claude mapping %q %v", got, err)
	}
	var cc activityCursor
	if err := readActivityAppend(claudePath, &cc, parseClaudeActivity); err != nil || cc.Tokens.Total != 5 {
		t.Fatalf("Claude usage %+v %v", cc, err)
	}
	if err := os.WriteFile(claudePath, []byte("{\"type\":\"assistant\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}}\n{\"type\":\"assistant\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":4}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readActivityAppend(claudePath, &cc, parseClaudeActivity); err != nil || cc.Tokens.Total != 6 {
		t.Fatalf("Claude cumulative update %+v %v", cc, err)
	}
	if err := os.WriteFile(claudePath, []byte("{\"type\":\"assistant\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":4}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readActivityAppend(claudePath, &cc, parseClaudeActivity); err != nil || cc.Tokens.Total != 6 {
		t.Fatalf("Claude replay double counted %+v %v", cc, err)
	}
}

func TestActivityStates(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	threshold := activityThresholds{Working: 120 * time.Second, Hung: 10 * time.Minute, Loop: 5 * time.Minute, LoopCalls: 5, CrashProbe: 15 * time.Second}
	a := api.Agent{Status: api.AgentRunning}
	base := activityCursor{SeenTurn: true, LastEventAt: now.Add(-30 * time.Second), Pending: map[string]pendingActivityCall{}}
	check := func(name string, c activityCursor, owed int, tmux, process bool, probeErr error, want string) {
		t.Helper()
		got := activityState(&c, a, owed, tmux, process, probeErr, now, threshold)
		if got.State != want {
			t.Errorf("%s got %s want %s", name, got.State, want)
		}
	}
	check("working", base, 0, true, true, nil, "working")
	changed := base
	changed.LastEventAt = now.Add(-5 * time.Minute)
	changed.WorktreeChangedAt = now.Add(-10 * time.Second)
	check("recent worktree change", changed, 0, true, true, nil, "working")
	c := base
	c.Pending = map[string]pendingActivityCall{"x": {Name: "exec_command", Since: now.Add(-11 * time.Minute)}}
	check("hung", c, 0, true, true, nil, "hung_tool")
	c.Pending["x"] = pendingActivityCall{Name: "sleep", Since: now.Add(-11 * time.Minute), WaitUntil: now.Add(time.Minute)}
	check("planned wait", c, 0, true, true, nil, "working")
	waiting := a
	waiting.Status = api.AgentNeedsInput
	if got := activityState(&c, waiting, 1, true, true, nil, now, threshold); got.State != "unknown" || got.Reason != "planned wait or input required" {
		t.Fatalf("planned dependency classified as failure: %+v", got)
	}
	c = base
	c.TurnComplete = true
	check("silent", c, 1, true, true, nil, "finished_silent")
	check("idle", c, 0, true, true, nil, "idle")
	c = base
	c.MissingSince = now.Add(-16 * time.Second)
	check("crashed", c, 0, false, false, nil, "crashed")
	check("probe unknown", c, 0, false, false, fmt.Errorf("probe"), "unknown")
	c = base
	c.Unknown = true
	check("format unknown", c, 0, true, true, nil, "unknown")
	c = base
	c.Worktree = "same"
	for i := 0; i < 5; i++ {
		c.Completed = append(c.Completed, completedActivityCall{Signature: "tool:abc", At: now.Add(-time.Duration(5-i) * time.Minute), Tokens: int64(i + 1), Worktree: "same"})
	}
	check("looping", c, 0, true, true, nil, "looping")
	c.Completed[4].Worktree = "changed"
	check("worktree breaks loop", c, 0, true, true, nil, "working")
	c.Worktree = ""
	check("unknown worktree cannot prove loop", c, 0, true, true, nil, "working")
}

func TestActivityToolCallPairing(t *testing.T) {
	var codex activityCursor
	if err := parseCodexActivity([]byte(`{"type":"response_item","timestamp":"2026-09-25T20:00:00Z","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"synthetic\"}"}}`), &codex); err != nil || len(codex.Pending) != 1 {
		t.Fatalf("Codex call %+v %v", codex, err)
	}
	if err := parseCodexActivity([]byte(`{"type":"response_item","timestamp":"2026-09-25T20:00:01Z","payload":{"type":"function_call_output","call_id":"c1"}}`), &codex); err != nil || len(codex.Pending) != 0 || len(codex.Completed) != 1 {
		t.Fatalf("Codex result %+v %v", codex, err)
	}
	var claude activityCursor
	if err := parseClaudeActivity([]byte(`{"type":"assistant","timestamp":"2026-09-25T20:00:00Z","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"synthetic"}}]}}`), &claude); err != nil || len(claude.Pending) != 1 {
		t.Fatalf("Claude tool %+v %v", claude, err)
	}
	if err := parseClaudeActivity([]byte(`{"type":"user","timestamp":"2026-09-25T20:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}`), &claude); err != nil || len(claude.Pending) != 0 || len(claude.Completed) != 1 {
		t.Fatalf("Claude tool result %+v %v", claude, err)
	}
}

func TestActivityThresholdOverridesAreBounded(t *testing.T) {
	t.Setenv("TAILTERM_ACTIVITY_HUNG_SECONDS", "1200")
	t.Setenv("TAILTERM_ACTIVITY_LOOP_CALLS", "8")
	threshold := activityDefaults()
	if threshold.Hung != 20*time.Minute || threshold.LoopCalls != 8 {
		t.Fatalf("overrides %+v", threshold)
	}
	t.Setenv("TAILTERM_ACTIVITY_HUNG_SECONDS", "999999")
	if activityDefaults().Hung != 10*time.Minute {
		t.Fatal("unsafe override accepted")
	}
}

func TestActivityExplicitWaitFromCodexArguments(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	wait := explicitWait(json.RawMessage(`"{\"yield_time_ms\":900000}"`), now)
	if !wait.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("wait bound %s", wait)
	}
}

func TestActivityUnknownFormatIsBounded(t *testing.T) {
	if err := parseCodexActivity([]byte(`{"type":"future_record","payload":{}}`), &activityCursor{}); err == nil {
		t.Fatal("future Codex format silently classified")
	}
	if err := parseClaudeActivity([]byte(`{"type":"future_record","message":{}}`), &activityCursor{}); err == nil {
		t.Fatal("future Claude format silently classified")
	}
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	if err := os.WriteFile(path, []byte("{broken}\n"+strings.Repeat("x", maxActivityLine+1)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var c activityCursor
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || !c.Unknown {
		t.Fatalf("oversize must be bounded unknown: %+v %v", c, err)
	}
}

func TestActivityTickPostsOnlyTransitionsAndRetriesLostReply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "relay"))
	thread := "12345678-1234-1234-1234-123456789abc"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "25")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	secret := "PRIVATE_TRANSCRIPT_SENTINEL"
	transcript := `{"type":"task_started","timestamp":"2026-09-25T20:00:00Z"}` + "\n" + `{"type":"response_item","timestamp":"2026-09-25T20:00:00Z","payload":{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"PRIVATE_TRANSCRIPT_SENTINEL\"}"}}` + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	posts := 0
	requests := map[string]bool{}
	lostOnce := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/agents/agt_0123456789abcdef"):
			_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", Status: api.AgentRunning, Online: true, Session: "synthetic"})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/activity"):
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), secret) {
				t.Error("transcript content leaked")
			}
			var req api.ActivityReport
			if json.Unmarshal(body, &req) != nil {
				t.Error("invalid report")
			}
			if !requests[req.RequestID] {
				posts++
				requests[req.RequestID] = true
			}
			if !lostOnce {
				lostOnce = true
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(req.Activity)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	client, _ := api.NewClient(hub.URL, 5*time.Second)
	b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: thread, Runtime: "codex", Codex: filepath.Join(home, "codex")}
	probe := func(api.Agent) (bool, bool, error) { return true, true, nil }
	if err := relayActivityTick(context.Background(), b, client, now, probe); err == nil {
		t.Fatal("expected lost reply")
	}
	if err := relayActivityTick(context.Background(), b, client, now.Add(time.Second), probe); err != nil || posts != 1 {
		t.Fatalf("retry ignored spacing: posts=%d err=%v", posts, err)
	}
	if err := relayActivityTick(context.Background(), b, client, now.Add(16*time.Second), probe); err != nil {
		t.Fatal(err)
	}
	if err := relayActivityTick(context.Background(), b, client, now.Add(32*time.Second), probe); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("change-only posts=%d", posts)
	}
}

func TestActivityCLIShowsAgentAndTeamSnapshots(t *testing.T) {
	task := "tsk_0123456789abcdef"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/agents") {
			_ = json.NewEncoder(w).Encode(api.AgentList{Agents: []api.Agent{{ID: "agt_0123456789abcdef", TaskID: task, Name: "builder", Session: "fake", Host: "mini", Status: api.AgentRunning, Activity: &api.AgentActivity{State: "hung_tool", ObservedAt: time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC), PendingTool: "exec_command", Tokens: api.TokenTotals{Total: 120}}}}})
		} else if strings.HasSuffix(r.URL.Path, "/team-queue") {
			_ = json.NewEncoder(w).Encode(api.TeamQueueList{ConcurrencyLimit: 1, Entries: []api.TeamQueueEntry{{ID: "tqe_0123456789abcdef", TaskID: task, ItemID: "wi_0123456789abcdef", State: "running", Tokens: api.TokenTotals{Total: 120}, Activities: []api.TeamAgentActivity{{Name: "builder", Activity: &api.AgentActivity{State: "hung_tool"}}}}}})
		} else {
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: task}
	agents, err := captureRelayOutput(t, false, func() error { return cmdAgents(e, nil) })
	if err != nil || !strings.Contains(agents, "activity=hung_tool") || !strings.Contains(agents, "last-transition tokens=120") {
		t.Fatalf("agents output %q %v", agents, err)
	}
	queue, err := captureRelayOutput(t, false, func() error { return cmdTeamQueue(e, []string{"list"}) })
	if err != nil || !strings.Contains(queue, "team last-transition tokens=120") || !strings.Contains(queue, "builder activity=hung_tool") {
		t.Fatalf("queue output %q %v", queue, err)
	}
}

func TestActivityRealShapedCodexTurnsAndInputs(t *testing.T) {
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	var c activityCursor
	for _, line := range []string{
		`{"type":"event_msg","timestamp":"2026-09-25T20:00:00Z","payload":{"type":"task_started"}}`,
		`{"type":"response_item","timestamp":"2026-09-25T20:00:01Z","payload":{"type":"custom_tool_call","name":"exec_command","call_id":"one","input":"{\"cmd\":\"first\"}"}}`,
		`{"type":"response_item","timestamp":"2026-09-25T20:00:02Z","payload":{"type":"custom_tool_call_output","call_id":"one"}}`,
		`{"type":"response_item","timestamp":"2026-09-25T20:00:03Z","payload":{"type":"custom_tool_call","name":"exec_command","call_id":"two","input":"{\"cmd\":\"second\",\"yield_time_ms\":900000}"}}`,
		`{"type":"event_msg","timestamp":"2026-09-25T20:00:04Z","payload":{"type":"task_complete"}}`,
	} {
		if err := parseCodexActivity([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
	}
	if !c.SeenTurn || !c.TurnComplete || len(c.Completed) != 1 || len(c.Pending) != 1 || c.Completed[0].Signature == c.Pending["two"].Signature || !c.Pending["two"].WaitUntil.Equal(now.Add(3*time.Second+15*time.Minute)) {
		t.Fatalf("real-shaped Codex evidence: %+v", c)
	}
	if got := activityState(&c, api.Agent{Status: api.AgentDone}, 0, true, true, nil, now.Add(5*time.Second), activityDefaults()); got.State != "idle" {
		t.Fatalf("idle: %+v", got)
	}
	if got := activityState(&c, api.Agent{Status: api.AgentDone}, 1, true, true, nil, now.Add(5*time.Second), activityDefaults()); got.State != "finished_silent" {
		t.Fatalf("silent: %+v", got)
	}
}

func TestActivityFormatRecoveryAndPlannedWaitLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	content := "{bad}\n" + `{"type":"compacted","payload":{}}` + "\n" + strings.Repeat("x", maxActivityLine+1) + "\n" + `{"type":"event_msg","timestamp":"2026-09-25T20:00:00Z","payload":{"type":"task_started"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var c activityCursor
	for i := 0; i < 2; i++ {
		if err := readActivityAppend(path, &c, parseCodexActivity); err != nil {
			t.Fatal(err)
		}
	}
	if c.Unknown || !c.SeenTurn {
		t.Fatalf("failed to recover after unknown records: %+v", c)
	}
	if got := explicitWait(json.RawMessage(`{"timeout":720000}`), time.Unix(0, 0)); !got.Equal(time.Unix(0, 0).Add(12 * time.Minute)) {
		t.Fatalf("Claude Bash wait: %s", got)
	}
	now := time.Date(2026, 9, 25, 20, 20, 0, 0, time.UTC)
	c.LastEventAt, c.Worktree = now, "same"
	for i := 0; i < 5; i++ {
		c.Completed = append(c.Completed, completedActivityCall{Signature: "wait", At: now.Add(-time.Duration(5-i) * time.Minute), Tokens: int64(i + 1), Worktree: "same", PlannedWait: true})
	}
	if got := activityState(&c, api.Agent{Status: api.AgentRunning}, 0, true, true, nil, now, activityDefaults()); got.State == "looping" {
		t.Fatalf("planned waits looped: %+v", got)
	}
	c.Pending = map[string]pendingActivityCall{"wait": {Name: "exec_command", Since: now.Add(-9 * time.Minute), Signature: "wait"}}
	finishActivityCall(&c, "wait", now)
	if !c.Completed[len(c.Completed)-1].PlannedWait {
		t.Fatal("long completed wait was counted as a short repeated call")
	}
}

func TestActivityConflictDoesNotStarveObservation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv("HOME", home)
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	gets, posts := 0, 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			gets++
			_ = json.NewEncoder(w).Encode(api.Agent{RunID: "run_0123456789abcdef", Status: api.AgentRunning, Online: true, Session: "fake"})
			return
		}
		if r.Method == "POST" {
			posts++
			w.WriteHeader(http.StatusConflict)
			return
		}
		http.NotFound(w, r)
	}))
	defer hub.Close()
	client, _ := api.NewClient(hub.URL, time.Second)
	b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: "12345678-1234-1234-1234-123456789abc", Runtime: "codex", CreatedAt: now}
	for i := 0; i < 20; i++ {
		if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(i)*16*time.Second), func(api.Agent) (bool, bool, error) { return true, true, nil }); err != nil {
			t.Fatal(err)
		}
	}
	if gets != 20 || posts != 1 {
		t.Fatalf("conflict starved observation or retried permanently: gets=%d posts=%d", gets, posts)
	}
}

func TestActivityManyStaleBindingsSkipTranscriptAndFutureReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
	t.Setenv("HOME", home)
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	gets := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		_ = json.NewEncoder(w).Encode(api.Agent{RunID: "run_0123456789abcdef", Status: api.AgentClosed})
	}))
	defer hub.Close()
	client, _ := api.NewClient(hub.URL, time.Second)
	for i := 0; i < 40; i++ {
		b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: fmt.Sprintf("agt_%016x", i), Run: "run_0123456789abcdef", Thread: fmt.Sprintf("12345678-1234-1234-1234-%012x", i), Runtime: "codex", CreatedAt: now}
		for pass := 0; pass < 2; pass++ {
			if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(pass)*16*time.Second), nil); err != nil {
				t.Fatal(err)
			}
		}
		var c activityCursor
		data, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".activity.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &c); err != nil || !c.Ineligible || c.Path != "" || c.Offset != 0 {
			t.Fatalf("stale binding scanned: %+v %v", c, err)
		}
	}
	if gets != 40 {
		t.Fatalf("stale rechecks=%d", gets)
	}
}

func TestActivityNativeProbeIgnoresHeartbeatGap(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	old := spawn.Tmux
	spawn.Tmux = fake
	defer func() { spawn.Tmux = old }()
	tmux, process, err := activityProbeNative(api.Agent{Session: "fake", Online: false})
	if err == nil || !tmux || process {
		t.Fatalf("heartbeat gap was not uncertain: %v %v %v", tmux, process, err)
	}
	c := activityCursor{SeenTurn: true, MissingSince: time.Now().Add(-time.Minute)}
	if got := activityState(&c, api.Agent{Status: api.AgentRunning}, 0, tmux, process, err, time.Now(), activityDefaults()); got.State != "unknown" || !c.MissingSince.IsZero() {
		t.Fatalf("uncertain probe retained crash evidence: %+v %+v", got, c)
	}
}

func TestActivityClaudeExactRetryAndPanicIsolation(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(t.TempDir(), "state"))
	s := ownedSession{ID: "$1", Created: "123", Name: "fake", Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef"}
	originalRun := s.Run
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Agent{ID: s.Agent, RunID: originalRun, Runtime: "claude", Session: s.Name, Status: api.AgentRunning})
	}))
	defer hub.Close()
	s.Hub = hub.URL
	client, _ := api.NewClient(hub.URL, time.Second)
	if err := retryClaudeBinding(context.Background(), s, client); err != nil {
		t.Fatal(err)
	}
	id, _ := claudeSessionID(s.Agent)
	b := runtimeBinding{Hub: s.Hub, Agent: s.Agent}
	var saved runtimeBinding
	data, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".binding.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &saved); err != nil || saved.Thread != id || saved.Run != s.Run || saved.Runtime != "claude" || !validBinding(saved) {
		t.Fatalf("unsafe Claude retry: %+v %v", saved, err)
	}
	s.Run = "run_fedcba9876543210"
	if err := retryClaudeBinding(context.Background(), s, client); err == nil {
		t.Fatal("accepted mismatched run")
	}
	if err := runActivitySafely(func() error { panic("private transcript sentinel") }); err == nil || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("panic escaped or leaked: %v", err)
	}
}

func TestActivityWorktreeProbeIsReadOnlyAndSharedPerPass(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "git")
	calls := filepath.Join(home, "calls")
	content := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + calls + "'\nif [ \"$4\" = 'rev-parse' ]; then echo head; fi\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	cache := activityWorktreeCache{}
	ctx := context.WithValue(context.Background(), activityWorktreeContextKey{}, cache)
	for _, cwd := range []string{home, home, filepath.Join(home, "other")} {
		if _, err := cachedActivityWorktree(ctx, cwd); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected two probes for two distinct cwds, got %d: %q", len(lines), data)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "--no-optional-locks ") {
			t.Fatalf("mutating git probe: %s", line)
		}
	}
}
