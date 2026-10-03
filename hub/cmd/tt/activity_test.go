package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
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
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
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
		if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(i)*16*time.Second), func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }); err != nil {
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

func TestClaudeUserTextClearsCompletedTurn(t *testing.T) {
	var c activityCursor
	if err := parseClaudeActivity([]byte(`{"type":"assistant","message":{"stop_reason":"end_turn"}}`), &c); err != nil || !c.TurnComplete {
		t.Fatalf("completed turn: %+v %v", c, err)
	}
	if err := parseClaudeActivity([]byte(`{"type":"user","message":{"content":"new human prompt"}}`), &c); err != nil || c.TurnComplete {
		t.Fatalf("new text did not clear completion: %+v %v", c, err)
	}
	if err := parseClaudeActivity([]byte(`{"type":"assistant","message":{"stop_reason":"end_turn"}}`), &c); err != nil || !c.TurnComplete {
		t.Fatalf("second completion: %+v %v", c, err)
	}
	if err := parseClaudeActivity([]byte(`{"type":"mode","mode":"default","sessionId":"00000000-0000-4000-8000-000000000001"}`), &c); err != nil || !c.TurnComplete {
		t.Fatalf("mode record changed turn boundary: %+v %v", c, err)
	}
	if err := parseClaudeActivity([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool"}]}}`), &c); err != nil || !c.TurnComplete {
		t.Fatalf("tool result changed turn boundary: %+v %v", c, err)
	}
	for _, record := range []string{
		`{"type":"user","isMeta":true,"message":{"content":"<local-command-caveat>local command</local-command-caveat>"}}`,
		`{"type":"user","message":{"content":"<command-name>/model</command-name>\n<command-message>model</command-message>"}}`,
		`{"type":"user","message":{"content":"<local-command-stdout>Set model to opus</local-command-stdout>"}}`,
		`{"type":"user","isCompactSummary":true,"message":{"content":"previous context summary"}}`,
	} {
		if err := parseClaudeActivity([]byte(record), &c); err != nil || !c.TurnComplete {
			t.Fatalf("local or summary record changed completion: %+v %v", c, err)
		}
	}
	if err := parseClaudeActivity([]byte(`{"type":"user","message":{"content":"new real prompt"}}`), &c); err != nil || c.TurnComplete {
		t.Fatalf("real prompt did not clear completion: %+v %v", c, err)
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

func TestActivityLargeTranscriptFirstReport(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", home)
			t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
			thread := "12345678-1234-1234-1234-123456789abc"
			dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "25")
			name := "rollout-test-" + thread + ".jsonl"
			if runtime == "claude" {
				dir = filepath.Join(home, ".claude", "projects", "synthetic")
				name = thread + ".jsonl"
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name)
			var data strings.Builder
			if runtime == "codex" {
				data.WriteString(`{"type":"task_started","timestamp":"2026-09-25T20:00:00Z"}` + "\n")
			}
			for i := 0; i < 150; i++ {
				if runtime == "codex" {
					fmt.Fprintf(&data, "{\"type\":\"event_msg\",\"timestamp\":\"2026-09-25T20:00:00Z\",\"padding\":%q,\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":%d,\"output_tokens\":%d,\"total_tokens\":%d}}}}\n", strings.Repeat("x", 64<<10), i+1, i+1, 2*(i+1))
				} else {
					fmt.Fprintf(&data, "{\"type\":\"assistant\",\"timestamp\":\"2026-09-25T20:00:00Z\",\"padding\":%q,\"message\":{\"id\":\"m%d\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n", strings.Repeat("x", 64<<10), i)
				}
			}
			final := `{"type":"task_started","timestamp":"2026-09-25T20:01:00Z"}` + "\n"
			if runtime == "claude" {
				final = `{"type":"assistant","timestamp":"2026-09-25T20:01:00Z","message":{"id":"last"}}` + "\n"
			}
			data.WriteString(strings.TrimSuffix(final, "\n"))
			if data.Len() <= 2*maxActivityPass {
				t.Fatal("fixture too small")
			}
			if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
				t.Fatal(err)
			}
			var reports []api.ActivityReport
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					json.NewEncoder(w).Encode(api.Agent{RunID: "run_0123456789abcdef", Status: api.AgentRunning})
					return
				}
				var report api.ActivityReport
				if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
					t.Error(err)
				}
				reports = append(reports, report)
				json.NewEncoder(w).Encode(report.Activity)
			}))
			defer hub.Close()
			client, _ := api.NewClient(hub.URL, time.Second)
			b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: thread, Runtime: runtime}
			now := time.Date(2026, 9, 25, 20, 1, 0, 0, time.UTC)
			probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
			for pass := 0; pass < 3; pass++ {
				if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(pass)*16*time.Second), probe); err != nil {
					t.Fatal(err)
				}
				var c activityCursor
				saved, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".activity.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(saved, &c); err != nil {
					t.Fatal(err)
				}
				if c.Offset > int64((pass+1)*maxActivityPass) {
					t.Fatal("pass budget exceeded")
				}
				if len(reports) != 0 || c.Transition != 0 || c.LastState != "" {
					t.Fatalf("stale bootstrap post on pass %d: reports=%d transition=%d", pass, len(reports), c.Transition)
				}
			}
			// Each tick reloads its persisted cursor: restart during catch-up and
			// at a partial final record must retain totals and bootstrap state.
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.WriteString("\n"); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if err := relayActivityTick(context.Background(), b, client, now.Add(48*time.Second), probe); err != nil {
				t.Fatal(err)
			}
			if len(reports) != 1 || !reports[0].Activity.LastEventAt.Equal(now) || reports[0].Activity.Tokens != (api.TokenTotals{Input: 150, Output: 150, Total: 300}) || reports[0].Activity.State != "working" {
				t.Fatalf("first report not current: %+v", reports)
			}
			// Rotation reboots readiness without changing the saved transition
			// until the replacement transcript's final complete turn is reached.
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			end := `{"type":"task_complete","timestamp":"2026-09-25T20:02:00Z"}` + "\n"
			if runtime == "claude" {
				end = `{"type":"assistant","timestamp":"2026-09-25T20:02:00Z","message":{"id":"last","stop_reason":"end_turn"}}` + "\n"
			}
			replacement := strings.TrimSuffix(data.String(), strings.TrimSuffix(final, "\n")) + end
			if err := os.WriteFile(path, []byte(replacement), 0600); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 3; pass++ {
				if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(pass+4)*16*time.Second), probe); err != nil {
					t.Fatal(err)
				}
				if pass < 2 && len(reports) != 1 {
					t.Fatal("rotation published during catch-up")
				}
			}
			if len(reports) != 2 || reports[1].Activity.State != "idle" || !reports[1].Activity.LastEventAt.Equal(now.Add(time.Minute)) || reports[1].Activity.Tokens != (api.TokenTotals{Input: 150, Output: 150, Total: 300}) {
				t.Fatalf("rotation report: %+v", reports)
			}

		})
	}
}

func TestActivityReaderReadinessResetsAtObservedEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	// More than two passes, including an oversized skipped record at the end.
	initial := `{"type":"task_started","timestamp":"2026-09-25T20:00:00Z"}` + "\n"
	content := initial + strings.Repeat("x", 2*maxActivityPass+10)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var c activityCursor
	for pass := 0; pass < 3; pass++ {
		offset := c.Offset
		if err := readActivityAppend(path, &c, parseCodexActivity); err != nil {
			t.Fatal(err)
		}
		if c.Ready || c.Offset-offset > maxActivityPass {
			t.Fatalf("premature readiness or budget: pass%d %+v", pass, c)
		}
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		c = activityCursor{}
		if err = json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
	}
	appendLine := func(line string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err = f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	appendLine("\n" + `{"type":"task_complete","timestamp":"2026-09-25T20:01:00Z"}` + "\n")
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || !c.Ready || !c.TurnComplete {
		t.Fatalf("completed skip %+v %v", c, err)
	}
	appendLine(initial + strings.Repeat("x", maxActivityPass+10))
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || c.Ready {
		t.Fatalf("append readiness %+v %v", c, err)
	}
	// Truncation clears pending skip and readiness, then replays complete records.
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || !c.Ready || c.TurnComplete || c.Skipping {
		t.Fatalf("truncate %+v %v", c, err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(initial+strings.Repeat("x", maxActivityPass+10)), 0600); err != nil {
		t.Fatal(err)
	}
	oldID := c.FileID
	if err := readActivityAppend(path, &c, parseCodexActivity); err != nil || c.Ready || c.FileID == oldID || c.Offset != maxActivityPass {
		t.Fatalf("rotation %+v %v", c, err)
	}
	other := path + ".new"
	if err := os.WriteFile(other, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readActivityAppend(other, &c, parseCodexActivity); err != nil || !c.Ready || c.Path != other || c.Skipping {
		t.Fatalf("path reset %+v %v", c, err)
	}
}

func TestActivityIncompleteOrUnreadableTranscriptReportsHealth(t *testing.T) {
	for _, mode := range []string{"partial-dead", "unreadable-dead", "unreadable-alive", "partial-probe-error", "partial-dead-alive-dead"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", home)
			t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
			thread := "12345678-1234-1234-1234-123456789abc"
			dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "25")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
			data := `{"type":"task_started","timestamp":"2026-09-25T20:00:00Z"}` + "\n" + `{"type":"event_msg","timestamp":"2026-09-25T20:00:30Z","payload":{"type":"tok`
			if strings.HasPrefix(mode, "unreadable") {
				// A directory stats/discovers like a transcript but deterministically fails
				// ReadBytes with EISDIR, even when tests run as root.
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var reports []api.ActivityReport
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					json.NewEncoder(w).Encode(api.Agent{RunID: "run_0123456789abcdef", Status: api.AgentRunning})
					return
				}
				var report api.ActivityReport
				if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
					t.Error(err)
				}
				reports = append(reports, report)
				json.NewEncoder(w).Encode(report.Activity)
			}))
			defer hub.Close()
			client, _ := api.NewClient(hub.URL, time.Second)
			b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: thread, Runtime: "codex"}
			now := time.Date(2026, 9, 25, 20, 1, 0, 0, time.UTC)
			probes := 0
			probe := func(runtimeBinding, api.Agent) (bool, bool, error) {
				probes++
				if mode == "partial-probe-error" {
					return true, false, fmt.Errorf("synthetic permission")
				}
				return true, mode == "unreadable-alive" || (mode == "partial-dead-alive-dead" && probes == 2), nil
			}
			for pass := 0; pass < 4; pass++ {
				if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(pass)*16*time.Second), probe); err != nil {
					t.Fatal(err)
				}
			}
			want := "crashed"
			if mode == "unreadable-alive" || mode == "partial-probe-error" {
				want = "unknown"
			}
			if probes != 4 || len(reports) == 0 || reports[len(reports)-1].Activity.State != want {
				t.Fatalf("suppressed health: probes=%d reports=%+v", probes, reports)
			}
			if reports[0].Activity.State != "unknown" {
				t.Fatal("two-probe rule lost")
			}
			crashAt := now.Add(16 * time.Second)
			if mode == "partial-dead-alive-dead" {
				crashAt = now.Add(48 * time.Second)
			}
			if want == "crashed" && (len(reports) != 2 || reports[1].Activity.ObservedAt != crashAt) {
				t.Fatalf("crash timing: %+v", reports)
			}
			for _, r := range reports {
				if !r.Activity.LastEventAt.IsZero() || r.Activity.Tokens != (api.TokenTotals{}) {
					t.Fatalf("incomplete transcript data published: %+v", r)
				}
			}
		})
	}
}

func TestActivityHealthRetainsOnlyVerifiedSnapshot(t *testing.T) {
	for _, mode := range []string{"partial", "read-error", "appended-backlog", "rotation", "truncation", "new-path", "rotation-complete-without-usage"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CODEX_HOME", home)
			t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "state"))
			thread := "12345678-1234-1234-1234-123456789abc"
			dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "25")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "rollout-test-"+thread+".jsonl")
			initial := `{"type":"task_started","timestamp":"2026-09-25T20:00:00Z"}` + "\n" + `{"type":"event_msg","timestamp":"2026-09-25T20:00:10Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":200,"total_tokens":300}}}}` + "\n"
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			var reports []api.ActivityReport
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					json.NewEncoder(w).Encode(api.Agent{RunID: "run_0123456789abcdef", Status: api.AgentRunning})
					return
				}
				var report api.ActivityReport
				if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
					t.Error(err)
				}
				reports = append(reports, report)
				json.NewEncoder(w).Encode(report.Activity)
			}))
			defer hub.Close()
			client, _ := api.NewClient(hub.URL, time.Second)
			b := runtimeBinding{Hub: hub.URL, Task: "tsk_0123456789abcdef", Agent: "agt_0123456789abcdef", Run: "run_0123456789abcdef", Thread: thread, Runtime: "codex"}
			now := time.Date(2026, 9, 25, 20, 1, 0, 0, time.UTC)
			if err := os.Chtimes(path, now, now); err != nil {
				t.Fatal(err)
			}
			alive := true
			probeError := false
			probe := func(runtimeBinding, api.Agent) (bool, bool, error) {
				if probeError {
					return true, true, fmt.Errorf("synthetic probe failure")
				}
				return true, alive, nil
			}
			tick := func(pass int) {
				t.Helper()
				if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(pass)*16*time.Second), probe); err != nil {
					t.Fatal(err)
				}
			}
			tick(0)
			totals := api.TokenTotals{Input: 100, Output: 200, Total: 300}
			stamp := now.Add(-50 * time.Second)
			if len(reports) != 1 || reports[0].Activity.Tokens != totals || !reports[0].Activity.LastEventAt.Equal(stamp) {
				t.Fatalf("establish accurate report: %+v", reports)
			}
			if mode == "rotation-complete-without-usage" {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"type":"task_started","timestamp":"2026-09-25T20:00:20Z"}`+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				tick(1) // Complete EOF alone must not certify old retained usage as new.
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.WriteString(`{"type":"event_msg"`); err != nil {
					t.Fatal(err)
				}
				f.Close()
			} else if mode == "read-error" {
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
				// Ensure this exercises a real open failure instead of silently passing as root.
				if f, err := os.Open(path); err == nil {
					f.Close()
					t.Skip("host privilege bypasses mode0000; deterministic fresh-read-error case covers reporting")
				}
				defer os.Chmod(path, 0600)
			} else {
				tail := `{"type":"event_msg","timestamp":"2026-09-25T20:00:30Z","payload":{"type":"tok`
				if mode != "partial" {
					tail = `{"type":"event_msg","timestamp":"2026-09-25T20:00:20Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":999,"output_tokens":999,"total_tokens":1998}}}}` + "\n" + strings.Repeat("x", 3*maxActivityPass)
				}
				if mode == "rotation" {
					if err := os.Rename(path, path+".old"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(tail), 0600); err != nil {
						t.Fatal(err)
					}
				} else if mode == "truncation" {
					if err := os.WriteFile(path, []byte(tail[:40]), 0600); err != nil {
						t.Fatal(err)
					}
				} else if mode == "new-path" {
					path = filepath.Join(dir, "rollout-new-"+thread+".jsonl")
					if err := os.WriteFile(path, []byte(tail), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(path, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
				} else {
					f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.WriteString(tail); err != nil {
						t.Fatal(err)
					}
					f.Close()
				}
			}
			// An uncertain probe yields unknown first. Then a confirmed dead process
			// yields crashed only after two exact probes. Every tick reloads saved state.
			probeError = true
			tick(4)
			probeError = false
			alive = false
			tick(5)
			tick(6)
			if len(reports) != 3 || reports[1].Activity.State != "unknown" || reports[2].Activity.State != "crashed" {
				t.Fatalf("health transitions: %+v", reports)
			}
			retain := mode == "partial" || mode == "read-error" || mode == "appended-backlog"
			for _, r := range reports[1:] {
				if retain {
					if r.Activity.Tokens != totals || !r.Activity.LastEventAt.Equal(stamp) {
						t.Fatalf("verified snapshot lost/replaced by partial data: %+v", r)
					}
				} else if r.Activity.Tokens != (api.TokenTotals{}) || !r.Activity.LastEventAt.IsZero() {
					t.Fatalf("reset retained old/unverified snapshot: %+v", r)
				}
			}
		})
	}
}

// stuckHub is a fake hub for stuck-state ticks. It serves one agent and
// records each distinct activity report; rejectStuck answers 400 to the stuck
// state, like a hub from before it existed.
type stuckHub struct {
	t           *testing.T
	agent       api.Agent
	rejectStuck bool
	// rejectState answers 400 to one more state; rejected counts those.
	rejectState string
	rejected    int
	reports     []api.AgentActivity
	seen        map[string]bool
}

func (h *stuckHub) serve() *httptest.Server {
	h.seen = map[string]bool{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/agents/"+h.agent.ID):
			_ = json.NewEncoder(w).Encode(h.agent)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/obligations"):
			_ = json.NewEncoder(w).Encode(api.ObligationList{})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/activity"):
			var req api.ActivityReport
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				h.t.Error("invalid report")
			}
			if h.rejectStuck && req.Activity.State == "stuck" {
				http.Error(w, "invalid activity", http.StatusBadRequest)
				return
			}
			if h.rejectState != "" && req.Activity.State == h.rejectState {
				h.rejected++
				http.Error(w, "invalid activity", http.StatusBadRequest)
				return
			}
			if !h.seen[req.RequestID] {
				h.seen[req.RequestID] = true
				h.reports = append(h.reports, req.Activity)
			}
			_ = json.NewEncoder(w).Encode(req.Activity)
		default:
			http.NotFound(w, r)
		}
	}))
}

func stuckFixture(t *testing.T, runtime string) (runtimeBinding, *stuckHub, *api.Client, func(time.Time) error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "relay"))
	thread := "12345678-1234-1234-1234-123456789abc"
	var path, transcript string
	if runtime == "claude" {
		path = filepath.Join(home, ".claude", "projects", "p", thread+".jsonl")
		transcript = `{"type":"assistant","message":{"stop_reason":"end_turn"}}` + "\n"
	} else {
		path = filepath.Join(home, ".codex", "sessions", "2026", "09", "29", "rollout-test-"+thread+".jsonl")
		transcript = `{"type":"task_started","timestamp":"2026-09-29T20:00:00Z"}` + "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	h := &stuckHub{t: t, agent: api.Agent{ID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", Status: api.AgentRunning, Online: true, Session: "stuck-agent", Runtime: runtime}}
	server := h.serve()
	t.Cleanup(server.Close)
	client, _ := api.NewClient(server.URL, 5*time.Second)
	b := runtimeBinding{Hub: server.URL, Task: "tsk_0123456789abcdef", Agent: h.agent.ID, Run: h.agent.RunID, Thread: thread, Runtime: runtime, Session: "stuck-agent", Codex: filepath.Join(home, "codex")}
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	tick := func(now time.Time) error { return relayActivityTick(context.Background(), b, client, now, probe) }
	return b, h, client, tick
}

func stubPaneSize(t *testing.T, cols, rows *int) {
	t.Helper()
	previous := activityPaneSize
	activityPaneSize = func(_ context.Context, session string) (int, int, error) {
		if session != "stuck-agent" {
			t.Errorf("pane size read for %q", session)
		}
		return *cols, *rows, nil
	}
	t.Cleanup(func() { activityPaneSize = previous })
}

func TestActivityStuckOnTinyPane(t *testing.T) {
	cols, rows := 16, 1
	stubPaneSize(t, &cols, &rows)
	_, h, _, tick := stuckFixture(t, "codex")
	now := time.Date(2026, 9, 29, 20, 0, 5, 0, time.UTC)
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].State != "stuck" || h.reports[0].Reason != "pane 16x1 below minimum 80x24" {
		t.Fatalf("tiny pane reports %+v", h.reports)
	}
	// Restored to the fixed size, the agent is working again.
	cols, rows = 200, 50
	if err := tick(now.Add(16 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 || h.reports[1].State != "working" {
		t.Fatalf("after resize %+v", h.reports)
	}
	// At the minimum exactly, the pane is usable.
	cols, rows = 80, 24
	if reason := activityStuckReason(context.Background(), runtimeBinding{Session: "stuck-agent", Runtime: "codex"}, api.Agent{}, "idle", now, activityDefaults()); reason != "" {
		t.Fatalf("80x24 flagged: %q", reason)
	}
}

func TestActivityStuckOnUnconfirmedWake(t *testing.T) {
	cols, rows := 200, 50
	stubPaneSize(t, &cols, &rows)
	b, h, _, tick := stuckFixture(t, "claude")
	now := time.Date(2026, 9, 29, 20, 10, 0, 0, time.UTC)
	intent := claudeWakeIntent{Run: b.Run, Thread: b.Thread, Session: b.Session, Phase: "uncertain", At: now.Add(-3 * time.Minute), FirstAt: now.Add(-3 * time.Minute), Attempts: 2, LastRetry: "retry 2/4: Enter on own unsubmitted text"}
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(claudeWakePath(b), intent); err != nil {
		t.Fatal(err)
	}
	// No unread input: an old unconfirmed wake alone is not stuck.
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].State != "idle" {
		t.Fatalf("no unread %+v", h.reports)
	}
	h.agent.Unread = 3
	if err := tick(now.Add(16 * time.Second)); err != nil {
		t.Fatal(err)
	}
	want := "Claude wake unconfirmed for 3m (retry 2/4): retry 2/4: Enter on own unsubmitted text"
	if len(h.reports) != 2 || h.reports[1].State != "stuck" || h.reports[1].Reason != want {
		t.Fatalf("unconfirmed wake reports %+v", h.reports)
	}
	// Under the threshold, or once confirmed, it is not stuck.
	intent.FirstAt = now.Add(-2 * time.Minute)
	_ = writePrivateJSON(claudeWakePath(b), intent)
	if reason := activityStuckReason(context.Background(), b, h.agent, "idle", now, activityDefaults()); reason != "" {
		t.Fatalf("2m flagged: %q", reason)
	}
	intent.FirstAt, intent.Phase = now.Add(-10*time.Minute), "confirmed"
	_ = writePrivateJSON(claudeWakePath(b), intent)
	if reason := activityStuckReason(context.Background(), b, h.agent, "idle", now, activityDefaults()); reason != "" {
		t.Fatalf("confirmed flagged: %q", reason)
	}
	// A working agent is making progress whatever its wake says.
	intent.Phase = "exhausted"
	_ = writePrivateJSON(claudeWakePath(b), intent)
	if reason := activityStuckReason(context.Background(), b, h.agent, "working", now, activityDefaults()); reason != "" {
		t.Fatalf("working flagged: %q", reason)
	}
	if reason := activityStuckReason(context.Background(), b, h.agent, "idle", now, activityDefaults()); !strings.Contains(reason, "exhausted") {
		t.Fatalf("exhausted reason %q", reason)
	}
	t.Setenv("TAILTERM_ACTIVITY_WAKE_STUCK_SECONDS", "900")
	if reason := activityStuckReason(context.Background(), b, h.agent, "idle", now, activityDefaults()); reason != "" {
		t.Fatalf("threshold override ignored: %q", reason)
	}
}

func TestActivityStuckFallsBackOnOldHub(t *testing.T) {
	cols, rows := 16, 1
	stubPaneSize(t, &cols, &rows)
	_, h, _, tick := stuckFixture(t, "codex")
	h.rejectStuck = true
	now := time.Date(2026, 9, 29, 20, 0, 5, 0, time.UTC)
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].State != "unknown" || h.reports[0].Reason != "stuck: pane 16x1 below minimum 80x24" {
		t.Fatalf("old hub fallback %+v", h.reports)
	}
	// The downgrade is remembered: no repeated 400 or duplicate report.
	if err := tick(now.Add(16 * time.Second)); err != nil || len(h.reports) != 1 {
		t.Fatalf("repeat %v %+v", err, h.reports)
	}
	// Later transitions still post.
	cols, rows = 200, 50
	if err := tick(now.Add(32 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 || h.reports[1].State != "working" {
		t.Fatalf("later transition blocked %+v", h.reports)
	}
}

func TestOwnerHelperOfflineActivity(t *testing.T) {
	now := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	threshold := activityDefaults()
	helper := api.Agent{Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning}
	var c activityCursor
	for _, at := range []time.Time{now, now.Add(threshold.CrashProbe), now.Add(10 * threshold.CrashProbe)} {
		got := activityState(&c, helper, 1, false, false, nil, at, threshold)
		if got.State != "unknown" || got.Reason != "owner session offline" {
			t.Fatalf("helper absent at %s: %+v", at, got)
		}
	}
	if got := activityState(&c, helper, 0, true, true, errors.New("runtime discovery unavailable"), now, threshold); got.State != "unknown" || got.Reason != "owner session offline" {
		t.Fatalf("helper unverified: %+v", got)
	}
	// The same absence is a crash for an ordinary agent.
	var ordinary activityCursor
	activityState(&ordinary, api.Agent{Status: api.AgentRunning}, 0, false, false, nil, now, threshold)
	if got := activityState(&ordinary, api.Agent{Status: api.AgentRunning}, 0, false, false, nil, now.Add(threshold.CrashProbe), threshold); got.State != "crashed" {
		t.Fatalf("ordinary agent %+v", got)
	}
	// Through the relay tick: offline is reported as unknown, never crashed or stuck.
	cols, rows := 16, 1
	stubPaneSize(t, &cols, &rows)
	b, h, client, _ := stuckFixture(t, "claude")
	b.Role, h.agent.Role = api.AgentRoleOwnerHelper, api.AgentRoleOwnerHelper
	absent := func(runtimeBinding, api.Agent) (bool, bool, error) { return false, false, nil }
	for i := range 3 {
		if err := relayActivityTick(context.Background(), b, client, now.Add(time.Duration(i)*20*time.Second), absent); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.reports) != 1 || h.reports[0].State != "unknown" || h.reports[0].Reason != "owner session offline" {
		t.Fatalf("offline reports %+v", h.reports)
	}
	// Live in a tiny pane: the owner sizes that terminal, so it is not stuck.
	live := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	if err := relayActivityTick(context.Background(), b, client, now.Add(time.Minute), live); err != nil {
		t.Fatal(err)
	}
	for _, r := range h.reports {
		if r.State == "stuck" || r.State == "crashed" {
			t.Fatalf("helper reported %+v", h.reports)
		}
	}
}

func TestOwnerHelperHeartbeat(t *testing.T) {
	agent := api.Agent{ID: api.NewID("agt"), TaskID: api.NewID("tsk"), RunID: api.NewID("run"), Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning, Session: "owner", Runtime: "claude"}
	var beats []api.PostEventRequest
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/events") {
			var req api.PostEventRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			beats = append(beats, req)
			_ = json.NewEncoder(w).Encode(api.Event{Kind: req.Kind})
			return
		}
		_ = json.NewEncoder(w).Encode(agent)
	}))
	defer hub.Close()
	c, _ := api.NewClient(hub.URL, 5*time.Second)
	b := runtimeBinding{Hub: hub.URL, Task: agent.TaskID, Agent: agent.ID, Run: agent.RunID, Thread: "12345678-1234-1234-1234-123456789abc", Runtime: "claude", Role: api.AgentRoleOwnerHelper, Session: "owner"}
	alive := true
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return alive, alive, nil }
	now := time.Date(2026, 9, 29, 20, 0, 0, 0, time.UTC)
	var p relayProgress
	tick := func(at time.Time) {
		t.Helper()
		if err := relayHelperHeartbeat(context.Background(), b, &p, c, at, probe); err != nil {
			t.Fatal(err)
		}
	}
	tick(now)
	tick(now.Add(10 * time.Second)) // spaced to 30 seconds
	if len(beats) != 1 || beats[0].Kind != api.EventHeartbeat || beats[0].AgentID != agent.ID || beats[0].RunID != agent.RunID {
		t.Fatalf("beats %+v", beats)
	}
	tick(now.Add(31 * time.Second))
	alive = false // session gone: no heartbeat, so the helper goes offline
	tick(now.Add(62 * time.Second))
	agent.RunID = api.NewID("run") // re-registered elsewhere: this binding never beats
	alive = true
	tick(now.Add(93 * time.Second))
	ordinary := b
	ordinary.Role = ""
	p = relayProgress{}
	if err := relayHelperHeartbeat(context.Background(), ordinary, &p, c, now.Add(200*time.Second), probe); err != nil {
		t.Fatal(err)
	}
	if len(beats) != 2 {
		t.Fatalf("beats %+v", beats)
	}
}

func TestOwnerHelperWakeText(t *testing.T) {
	b := runtimeBinding{Task: "tsk_0123456789abcdef", Role: api.AgentRoleOwnerHelper}
	messages := []api.Message{{Seq: 41, Text: "hi"}}
	inbox := claudeWakeFor(b, claudeWakePrompt(messages, "agt_0123456789abcdef"))
	broker := claudeWakeFor(b, claudeBrokerPrompt("Tailterm obligations #42", 42, "wake_0123456789abcdef"))
	if inbox != "Tailterm messages #41. Run tt helper inbox --task tsk_0123456789abcdef." ||
		broker != "Tailterm obligations #42. Run tt helper inbox --task tsk_0123456789abcdef. Wake wake_0123456789abcdef." {
		t.Fatalf("helper prompts %q %q", inbox, broker)
	}
	for _, prompt := range []string{inbox, broker} {
		if strings.ContainsAny(prompt, "\"'`$\n") {
			t.Fatalf("unsafe prompt %q", prompt)
		}
	}
	b.Role = ""
	if got := claudeWakeFor(b, claudeWakePrompt(messages, "agt_0123456789abcdef")); got != "Tailterm messages #41. Run tt inbox --unread --mark-read." {
		t.Fatalf("ordinary prompt changed %q", got)
	}
}

// Records in the shape Claude Code 2.1.285 wrote when a server error ended a
// turn mid-response (bug wi_132c8895adfe0886, order #15557): a real prompt,
// an end_turn text reply, the synthetic API-error record and turn_duration.
// Keys and values are as recorded; ids, text and times are synthetic.
var (
	claudeAPIErrorPrompt       = `{"parentUuid":"00000000-0000-4000-8000-0000000000a0","isSidechain":false,"promptId":"00000000-0000-4000-8000-0000000000af","type":"user","message":{"role":"user","content":"Synthetic fixture prompt before the API error."},"uuid":"00000000-0000-4000-8000-0000000000a1","timestamp":"2026-09-25T19:00:00.000Z","permissionMode":"bypassPermissions","origin":{"kind":"human"},"promptSource":"typed","userType":"external","entrypoint":"cli","sessionId":"11111111-1111-4111-8111-111111111111","version":"2.1.285"}`
	claudeAPIErrorEndTurn      = `{"parentUuid":"00000000-0000-4000-8000-0000000000a1","isSidechain":false,"type":"assistant","uuid":"00000000-0000-4000-8000-0000000000a2","timestamp":"2026-09-25T19:00:05.000Z","message":{"model":"claude-opus-5-5","id":"msg_fixture00000000000000a2","type":"message","role":"assistant","content":[{"type":"text","text":"Synthetic reply before the error."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"cache_creation_input_tokens":10,"cache_read_input_tokens":100,"output_tokens":8}},"requestId":"req_fixture00000000000000a2","userType":"external","entrypoint":"cli","sessionId":"11111111-1111-4111-8111-111111111111","version":"2.1.285"}`
	claudeAPIErrorRecord       = `{"parentUuid":"00000000-0000-4000-8000-0000000000a2","isSidechain":false,"type":"assistant","uuid":"00000000-0000-4000-8000-0000000000a3","timestamp":"2026-09-25T19:00:55.000Z","message":{"diagnostics":null,"id":"00000000-0000-4000-8000-0000000000a4","container":null,"model":"<synthetic>","role":"assistant","stop_details":null,"stop_reason":"stop_sequence","stop_sequence":"","type":"message","usage":{"output_tokens_details":null,"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0},"service_tier":null,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"inference_geo":null,"iterations":null,"speed":null,"fallback_credit":null},"content":[{"type":"text","text":"API Error: Server error mid-response. The response above may be incomplete."}],"context_management":null},"error":"server_error","truncatedAfterOutput":true,"isApiErrorMessage":true,"userType":"external","entrypoint":"cli","sessionId":"11111111-1111-4111-8111-111111111111","version":"2.1.285"}`
	claudeAPIErrorTurnDuration = `{"parentUuid":"00000000-0000-4000-8000-0000000000a3","isSidechain":false,"type":"system","subtype":"turn_duration","durationMs":55000,"messageCount":4,"timestamp":"2026-09-25T19:00:55.014Z","uuid":"00000000-0000-4000-8000-0000000000a5","isMeta":false,"userType":"external","entrypoint":"cli","sessionId":"11111111-1111-4111-8111-111111111111","version":"2.1.285"}`
	claudeAPIErrorToolUse      = `{"type":"assistant","timestamp":"2026-09-25T19:00:10.000Z","message":{"id":"msg_fixture00000000000000a6","role":"assistant","content":[{"type":"tool_use","id":"toolu_fixture_a6","name":"Bash","input":{"command":"synthetic"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}}`
)

// p1: an API-error record or a turn_duration record completes a Claude turn.
func TestClaudeAPIErrorEndsTurn(t *testing.T) {
	const reason = "turn ended by API error (server_error)"
	parse := func(c *activityCursor, records ...string) {
		t.Helper()
		for _, record := range records {
			if err := parseClaudeActivity([]byte(record), c); err != nil {
				t.Fatalf("parse %s: %v", record[:40], err)
			}
		}
	}
	t.Run("recorded sequence", func(t *testing.T) {
		var c activityCursor
		parse(&c, claudeAPIErrorPrompt, claudeAPIErrorEndTurn)
		if !c.TurnComplete || c.TurnEndReason != "" {
			t.Fatalf("end_turn: %+v", c)
		}
		tokens := c.Tokens
		parse(&c, claudeAPIErrorRecord)
		if !c.SeenTurn || !c.TurnComplete || c.TurnEndReason != reason {
			t.Fatalf("API-error record alone did not complete the turn: complete=%v reason=%q", c.TurnComplete, c.TurnEndReason)
		}
		if c.Tokens != tokens {
			t.Fatalf("synthetic zero-usage record changed tokens: %+v -> %+v", tokens, c.Tokens)
		}
		parse(&c, claudeAPIErrorTurnDuration)
		if !c.TurnComplete || c.TurnEndReason != reason {
			t.Fatalf("turn_duration lost completion or reason: complete=%v reason=%q", c.TurnComplete, c.TurnEndReason)
		}
		parse(&c, claudeAPIErrorPrompt)
		if c.TurnComplete || c.TurnEndReason != "" {
			t.Fatalf("new prompt did not clear completion and reason: complete=%v reason=%q", c.TurnComplete, c.TurnEndReason)
		}
		parse(&c, claudeAPIErrorEndTurn)
		if !c.TurnComplete || c.TurnEndReason != "" {
			t.Fatalf("normal end_turn kept a reason: %q", c.TurnEndReason)
		}
	})
	t.Run("turn_duration alone completes", func(t *testing.T) {
		var c activityCursor
		parse(&c, claudeAPIErrorPrompt, claudeAPIErrorToolUse)
		if c.TurnComplete || len(c.Pending) != 1 {
			t.Fatalf("tool use: complete=%v pending=%d", c.TurnComplete, len(c.Pending))
		}
		parse(&c, claudeAPIErrorTurnDuration)
		if !c.SeenTurn || !c.TurnComplete || len(c.Pending) != 0 || c.TurnEndReason != "" {
			t.Fatalf("turn_duration: complete=%v pending=%d reason=%q", c.TurnComplete, len(c.Pending), c.TurnEndReason)
		}
		// A later assistant record means the turn went on after all.
		parse(&c, claudeAPIErrorToolUse)
		if c.TurnComplete {
			t.Fatal("assistant record after turn_duration left the turn complete")
		}
	})
	t.Run("API error clears an orphaned tool call", func(t *testing.T) {
		var c activityCursor
		parse(&c, claudeAPIErrorPrompt, claudeAPIErrorToolUse, claudeAPIErrorRecord)
		if !c.TurnComplete || len(c.Pending) != 0 || c.TurnEndReason != reason {
			t.Fatalf("complete=%v pending=%d reason=%q", c.TurnComplete, len(c.Pending), c.TurnEndReason)
		}
	})
	t.Run("error code is bounded", func(t *testing.T) {
		for code, want := range map[string]string{
			"rate_limit":            "turn ended by API error (rate_limit)",
			"authentication_failed": "turn ended by API error (authentication_failed)",
			"Server Error: /secret": "turn ended by API error",
			strings.Repeat("a", 41): "turn ended by API error",
			"":                      "turn ended by API error",
		} {
			var c activityCursor
			record := strings.Replace(claudeAPIErrorRecord, `"error":"server_error"`, `"error":`+strconv.Quote(code), 1)
			parse(&c, record)
			if !c.TurnComplete || c.TurnEndReason != want {
				t.Fatalf("code %q: complete=%v reason=%q", code, c.TurnComplete, c.TurnEndReason)
			}
		}
	})
	t.Run("other system records do not complete", func(t *testing.T) {
		var c activityCursor
		parse(&c, claudeAPIErrorPrompt, claudeAPIErrorToolUse, strings.Replace(claudeAPIErrorTurnDuration, `"subtype":"turn_duration"`, `"subtype":"informational"`, 1))
		if c.TurnComplete || len(c.Pending) != 1 {
			t.Fatalf("informational: complete=%v pending=%d", c.TurnComplete, len(c.Pending))
		}
	})
}

// p3: after an API-error turn the agent is idle or finished_silent with the
// API-error reason, and the relay reports that reason to the hub.
func TestActivityAPIErrorTurnReason(t *testing.T) {
	const reason = "turn ended by API error (server_error)"
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	threshold := activityThresholds{Working: 120 * time.Second, Hung: 10 * time.Minute, Loop: 5 * time.Minute, LoopCalls: 5, CrashProbe: 15 * time.Second}
	a := api.Agent{Status: api.AgentRunning}
	var c activityCursor
	for _, record := range []string{claudeAPIErrorPrompt, claudeAPIErrorEndTurn, claudeAPIErrorRecord, claudeAPIErrorTurnDuration} {
		if err := parseClaudeActivity([]byte(record), &c); err != nil {
			t.Fatal(err)
		}
	}
	if got := activityState(&c, a, 0, true, true, nil, now, threshold); got.State != "idle" || got.Reason != reason {
		t.Fatalf("idle after API error: %+v", got)
	}
	if got := activityState(&c, a, 1, true, true, nil, now, threshold); got.State != "finished_silent" || got.Reason != reason {
		t.Fatalf("finished_silent after API error: %+v", got)
	}
	var normal activityCursor
	for _, record := range []string{claudeAPIErrorPrompt, claudeAPIErrorEndTurn} {
		if err := parseClaudeActivity([]byte(record), &normal); err != nil {
			t.Fatal(err)
		}
	}
	if got := activityState(&normal, a, 0, true, true, nil, now, threshold); got.State != "idle" || got.Reason != "" {
		t.Fatalf("idle after end_turn: %+v", got)
	}
	if got := activityState(&normal, a, 1, true, true, nil, now, threshold); got.State != "finished_silent" || got.Reason != "" {
		t.Fatalf("finished_silent after end_turn: %+v", got)
	}

	cols, rows := 200, 50
	stubPaneSize(t, &cols, &rows)
	b, h, _, tick := stuckFixture(t, "claude")
	path := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "p", b.Thread+".jsonl")
	transcript := strings.Join([]string{claudeAPIErrorPrompt, claudeAPIErrorEndTurn, claudeAPIErrorRecord, claudeAPIErrorTurnDuration}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].State != "idle" || h.reports[0].Reason != reason {
		t.Fatalf("relay reports %+v", h.reports)
	}
}

// Handler arms (wi_fc1396aef8a72a06): the relay reports an idle turn whose
// reason changes to an API rate limit, but not a stuck reason change.
func TestActivityReportReasonChangeReachesHub(t *testing.T) {
	idle := api.AgentActivity{State: "idle"}
	limited := api.AgentActivity{State: "idle", Reason: "turn ended by API error (rate_limit)"}
	if activityReportKey(idle) == activityReportKey(limited) {
		t.Fatal("idle reason is not part of the report key")
	}
	silent, silentLimited := idle, limited
	silent.State, silentLimited.State = "finished_silent", "finished_silent"
	if activityReportKey(silent) == activityReportKey(silentLimited) {
		t.Fatal("finished_silent reason is not part of the report key")
	}
	if activityReportKey(api.AgentActivity{State: "stuck", Reason: "a"}) != activityReportKey(api.AgentActivity{State: "stuck", Reason: "b"}) {
		t.Fatal("stuck reasons must not make new reports")
	}

	cols, rows := 200, 50
	stubPaneSize(t, &cols, &rows)
	b, h, _, tick := stuckFixture(t, "claude")
	path := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "p", b.Thread+".jsonl")
	clean := strings.Join([]string{claudeAPIErrorPrompt, claudeAPIErrorEndTurn}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(clean), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	record := strings.Replace(claudeAPIErrorRecord, `"error":"server_error"`, `"error":"rate_limit"`, 1)
	limitedTranscript := clean + strings.Join([]string{claudeAPIErrorPrompt, record, claudeAPIErrorTurnDuration}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(limitedTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tick(now.Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 || h.reports[0].State != "idle" || h.reports[0].Reason != "" || h.reports[1].State != "idle" || h.reports[1].Reason != "turn ended by API error (rate_limit)" {
		t.Fatalf("relay reports %+v", h.reports)
	}
	// The same idle reason again is not reported twice.
	if err := tick(now.Add(60 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 2 {
		t.Fatalf("repeat reported %+v", h.reports)
	}
}

// Review f3 (#15848): after an upgrade, a cursor an older relay saved (its key
// has no reason) must not make every idle binding send one extra report; the
// 09-24 relay 429 incident came from such a burst. The key upgrades in place,
// and a later rate-limit turn end is still reported.
func TestActivityReportKeyUpgradeSendsNoBurst(t *testing.T) {
	for _, first := range []string{"end_turn", "server_error"} {
		t.Run(first, func(t *testing.T) {
			cols, rows := 200, 50
			stubPaneSize(t, &cols, &rows)
			b, h, _, tick := stuckFixture(t, "claude")
			transcript := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "p", b.Thread+".jsonl")
			records := []string{claudeAPIErrorPrompt, claudeAPIErrorEndTurn}
			if first == "server_error" {
				records = append(records, claudeAPIErrorRecord, claudeAPIErrorTurnDuration)
			}
			text := strings.Join(records, "\n") + "\n"
			if err := os.WriteFile(transcript, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 25, 20, 0, 0, 0, time.UTC)
			if err := tick(now); err != nil {
				t.Fatal(err)
			}
			if len(h.reports) != 1 || h.reports[0].State != "idle" {
				t.Fatalf("first report %+v", h.reports)
			}
			// Rewrite the cursor as the previous relay saved it.
			cursorPath := filepath.Join(relayDir(), bindingKey(b)+".activity.json")
			data, err := os.ReadFile(cursorPath)
			if err != nil {
				t.Fatal(err)
			}
			var cursor map[string]any
			if err = json.Unmarshal(data, &cursor); err != nil {
				t.Fatal(err)
			}
			if legacy := legacyActivityKey(h.reports[0]); legacy == "" {
				delete(cursor, "lastWakeKey")
			} else {
				cursor["lastWakeKey"] = legacy
			}
			if data, err = json.Marshal(cursor); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(cursorPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err = tick(now.Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(h.reports) != 1 {
				t.Fatalf("upgrade sent an extra report %+v", h.reports)
			}
			var upgraded activityCursor
			if data, err = os.ReadFile(cursorPath); err != nil || json.Unmarshal(data, &upgraded) != nil || !strings.HasPrefix(upgraded.LastWakeKey, activityKeyVersion) {
				t.Fatalf("key not upgraded in place: %q %v", upgraded.LastWakeKey, err)
			}
			record := strings.Replace(claudeAPIErrorRecord, `"error":"server_error"`, `"error":"rate_limit"`, 1)
			text += strings.Join([]string{claudeAPIErrorPrompt, record, claudeAPIErrorTurnDuration}, "\n") + "\n"
			if err = os.WriteFile(transcript, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if err = tick(now.Add(60 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(h.reports) != 2 || h.reports[1].Reason != "turn ended by API error (rate_limit)" {
				t.Fatalf("rate limit after upgrade %+v", h.reports)
			}
		})
	}
}

// Provider blocks (wi_72f41bd375032cf0, docs/provider-blocked.md).

func providerFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "provider-blocked", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// providerCursor parses the first n records of a fixture (all when n < 0).
func providerCursor(t *testing.T, runtime, name string, n int) *activityCursor {
	t.Helper()
	parse := parseCodexActivity
	if runtime == "claude" {
		parse = parseClaudeActivity
	}
	c := &activityCursor{}
	for i, line := range strings.Split(strings.TrimSpace(providerFixture(t, name)), "\n") {
		if n >= 0 && i >= n {
			break
		}
		if err := parse([]byte(line), c); err != nil {
			t.Fatalf("%s record %d: %v", name, i, err)
		}
	}
	return c
}

func providerState(c *activityCursor) api.AgentActivity {
	return activityState(c, api.Agent{Status: api.AgentRunning}, 0, true, true, nil, time.Date(2026, 9, 26, 20, 0, 0, 0, time.UTC), activityDefaults())
}

func providerTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestActivityProviderBlockedClaude(t *testing.T) {
	for _, tc := range []struct {
		fixture, text, class, code string
		status                     int
		since                      string
	}{
		{"claude-fable-limit", "You've reached your Fable limit. Run /usage-credits to continue or switch models with /model.", "usage_limit", "rate_limit", 429, "2026-09-26T18:53:10Z"},
		{"claude-session-limit", "You've hit your session limit · resets 3pm (America/Los_Angeles)", "usage_limit", "rate_limit", 429, "2026-09-26T18:53:10Z"},
		{"claude-login-expired", "Login expired · Please run /login", "auth", "authentication_failed", 0, "2026-09-26T18:53:10Z"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			if !strings.Contains(providerFixture(t, tc.fixture), `"text":"`+tc.text+`"`) {
				t.Fatal("fixture does not carry the exact observed text")
			}
			got := providerState(providerCursor(t, "claude", tc.fixture, -1))
			p := got.Provider
			if got.State != "provider_blocked" || p == nil {
				t.Fatalf("state %+v", got)
			}
			want := api.ProviderBlock{Provider: "anthropic", Runtime: "claude", Model: "claude-fable-5-1", Class: tc.class, Code: tc.code, Status: tc.status, Since: providerTime(t, tc.since)}
			if !api.SameProviderBlock(p, &want) || !p.Valid() {
				t.Fatalf("block %+v, want %+v", *p, want)
			}
			if got.Reason != "" {
				t.Fatalf("reason %q", got.Reason)
			}
		})
	}
	// The first failed prompt alone already blocks for a usage limit, and the
	// block ranks above a planned wait and below a crash.
	c := providerCursor(t, "claude", "claude-fable-limit", 4)
	if got := providerState(c); got.State != "provider_blocked" || got.Provider.Class != "usage_limit" {
		t.Fatalf("first record %+v", got)
	}
	now := time.Date(2026, 9, 26, 20, 0, 0, 0, time.UTC)
	if got := activityState(c, api.Agent{Status: api.AgentNeedsInput}, 0, true, true, nil, now, activityDefaults()); got.State != "provider_blocked" {
		t.Fatalf("needs_input hid the block: %+v", got)
	}
	c.MissingSince = now.Add(-time.Minute)
	if got := activityState(c, api.Agent{Status: api.AgentRunning}, 0, false, false, nil, now, activityDefaults()); got.State != "crashed" || got.Provider != nil {
		t.Fatalf("crash must outrank the block: %+v", got)
	}
	// The server-error text is the fourth observed message; one such turn is
	// not a block (TestActivityProviderBlockedRepeat covers the rule).
	if !strings.Contains(providerFixture(t, "claude-server-error"), `"text":"API Error: Server error mid-response. The response above may be incomplete."`) {
		t.Fatal("server error fixture does not carry the exact observed text")
	}
	// Other API errors are not provider blocks and keep idle with a reason.
	other := strings.Replace(strings.Split(providerFixture(t, "claude-login-expired"), "\n")[3], `"error":"authentication_failed"`, `"error":"invalid_request"`, 1)
	var plain activityCursor
	if err := parseClaudeActivity([]byte(other), &plain); err != nil {
		t.Fatal(err)
	}
	if got := providerState(&plain); got.State != "idle" || got.Reason != "turn ended by API error (invalid_request)" || got.Provider != nil {
		t.Fatalf("invalid_request %+v", got)
	}
	// A malformed model name is never forwarded.
	var bad activityCursor
	for _, line := range strings.Split(strings.TrimSpace(strings.Replace(providerFixture(t, "claude-login-expired"), `"model":"claude-fable-5-1"`, `"model":"bad model/name"`, 1)), "\n") {
		if err := parseClaudeActivity([]byte(line), &bad); err != nil {
			t.Fatal(err)
		}
	}
	if got := providerState(&bad); got.Provider == nil || got.Provider.Model != "unknown" {
		t.Fatalf("malformed model %+v", got.Provider)
	}
}

func TestActivityProviderBlockedCodex(t *testing.T) {
	const observed = `unexpected status 401 Unauthorized: Incorrect API key provided: sk-fake-EXAMPLE-not-a-key. You can find your API key at https://platform.openai.com/account/api-keys., url: https://chatgpt.com/backend-api/codex/responses, cf-ray: 0000000000000000-SJC, request id: 00000000-0000-4000-8000-00000000f401`
	fixture := providerFixture(t, "codex-401")
	if !strings.Contains(fixture, `"message":"`+observed+`"`) {
		t.Fatal("fixture does not carry the observed 401 message")
	}
	cols, rows := 200, 50
	stubPaneSize(t, &cols, &rows)
	b, h, _, tick := stuckFixture(t, "codex")
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "09", "29", "rollout-test-"+b.Thread+".jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tick(time.Date(2026, 9, 25, 22, 45, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if len(h.reports) != 1 || h.reports[0].State != "provider_blocked" || h.reports[0].Provider == nil {
		t.Fatalf("reports %+v", h.reports)
	}
	want := api.ProviderBlock{Provider: "openai", Runtime: "codex", Model: "gpt-6-astra", Class: "auth", Code: "other", Status: 401, Since: providerTime(t, "2026-09-25T22:40:03Z")}
	if p := h.reports[0].Provider; !api.SameProviderBlock(p, &want) || !p.Valid() {
		t.Fatalf("block %+v, want %+v", *p, want)
	}
	report, err := json.Marshal(h.reports[0])
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".activity.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"report": report, "cursor": cursor} {
		for _, secret := range []string{"sk-", "Incorrect API key", "cf-ray", "request id", "Unauthorized", "platform.openai.com"} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("%s contains %q: %s", name, secret, data)
			}
		}
	}
	if !strings.Contains(string(cursor), `"providerBlock"`) {
		t.Fatalf("cursor lost the block: %s", cursor)
	}
	// A 403 is the same class.
	var c activityCursor
	for _, line := range strings.Split(strings.TrimSpace(strings.ReplaceAll(fixture, "unexpected status 401 Unauthorized", "unexpected status 403 Forbidden")), "\n") {
		if err := parseCodexActivity([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
	}
	if got := providerState(&c); got.State != "provider_blocked" || got.Provider.Class != "auth" || got.Provider.Status != 403 {
		t.Fatalf("403 %+v", got)
	}
}

func TestActivityProviderBlockedRepeat(t *testing.T) {
	// Claude: prompt, reply, then (prompt, error, turn_duration) twice, then a
	// prompt and an ordinary reply.
	one := providerCursor(t, "claude", "claude-server-error", 5)
	if got := providerState(one); got.State != "idle" || got.Reason != "turn ended by API error (server_error)" || got.Provider != nil {
		t.Fatalf("one server error %+v", got)
	}
	two := providerCursor(t, "claude", "claude-server-error", 8)
	got := providerState(two)
	if got.State != "provider_blocked" || got.Provider.Class != "server_error" || got.Provider.Code != "server_error" || got.Provider.Status != 0 || !got.Provider.Since.Equal(providerTime(t, "2026-09-26T18:53:10Z")) || got.Provider.Model != "claude-fable-5-1" {
		t.Fatalf("second server error %+v %+v", got, got.Provider)
	}
	// A new prompt alone does not clear it; an ordinary reply does.
	if got := providerState(providerCursor(t, "claude", "claude-server-error", 9)); got.State != "provider_blocked" {
		t.Fatalf("prompt cleared the block: %+v", got)
	}
	if got := providerState(providerCursor(t, "claude", "claude-server-error", -1)); got.State != "idle" || got.Reason != "" || got.Provider != nil {
		t.Fatalf("after recovery %+v", got)
	}
	// Codex: two header records, then (task_started, prompt, task_complete
	// with error) per failed turn.
	for _, tc := range []struct {
		fixture, class string
		status         int
	}{{"codex-503", "server_error", 503}, {"codex-429", "rate_limited", 429}} {
		if got := providerState(providerCursor(t, "codex", tc.fixture, 5)); got.State != "idle" || got.Reason != "turn ended by provider error ("+tc.class+")" || got.Provider != nil {
			t.Fatalf("%s one failure %+v", tc.fixture, got)
		}
		got := providerState(providerCursor(t, "codex", tc.fixture, 8))
		if got.State != "provider_blocked" || got.Provider.Class != tc.class || got.Provider.Status != tc.status || got.Provider.Provider != "openai" || got.Provider.Model != "gpt-6-astra" || !got.Provider.Since.Equal(providerTime(t, "2026-09-25T22:40:03Z")) {
			t.Fatalf("%s second failure %+v %+v", tc.fixture, got, got.Provider)
		}
	}
	// A prompt in the next turn keeps the block; model output clears it, and
	// so does an error-free task_complete.
	if got := providerState(providerCursor(t, "codex", "codex-503", 10)); got.State != "provider_blocked" {
		t.Fatalf("codex prompt cleared the block: %+v", got)
	}
	if c := providerCursor(t, "codex", "codex-503", 11); c.ProviderBlock != nil {
		t.Fatalf("codex model output kept the block: %+v", c.ProviderBlock)
	}
	if got := providerState(providerCursor(t, "codex", "codex-503", -1)); got.State != "idle" || got.Reason != "" || got.Provider != nil {
		t.Fatalf("codex after recovery %+v", got)
	}
	blocked := providerCursor(t, "codex", "codex-429", -1)
	if err := parseCodexActivity([]byte(`{"type":"event_msg","timestamp":"2026-09-25T23:00:00Z","payload":{"type":"task_complete","last_agent_message":"Synthetic fixture reply."}}`), blocked); err != nil || blocked.ProviderBlock != nil {
		t.Fatalf("error-free task_complete kept the block: %+v %v", blocked.ProviderBlock, err)
	}
	// A different class starts a new block with its own since.
	mixed := providerCursor(t, "codex", "codex-503", 5)
	if err := parseCodexActivity([]byte(`{"type":"event_msg","timestamp":"2026-09-25T22:50:00Z","payload":{"type":"task_complete","error":{"codex_error_info":"other","message":"unexpected status 429 Too Many Requests"}}}`), mixed); err != nil {
		t.Fatal(err)
	}
	if b := mixed.ProviderBlock; b == nil || b.Class != "rate_limited" || b.Count != 1 || !b.Since.Equal(providerTime(t, "2026-09-25T22:50:00Z")) {
		t.Fatalf("class change %+v", b)
	}
	if got := providerState(mixed); got.State != "idle" {
		t.Fatalf("class change counted the earlier class: %+v", got)
	}
	// The threshold is configurable and bounded.
	t.Setenv("TAILTERM_ACTIVITY_PROVIDER_REPEAT", "3")
	if got := providerState(two); got.State != "idle" {
		t.Fatalf("threshold 3 after two failures %+v", got)
	}
	three := providerCursor(t, "claude", "claude-server-error", 8)
	lines := strings.Split(providerFixture(t, "claude-server-error"), "\n")
	if err := parseClaudeActivity([]byte(lines[6]), three); err != nil {
		t.Fatal(err)
	}
	if got := providerState(three); got.State != "provider_blocked" || !got.Provider.Since.Equal(providerTime(t, "2026-09-26T18:53:10Z")) {
		t.Fatalf("threshold 3 after three failures %+v", got)
	}
	for _, value := range []string{"1", "21", "x"} {
		t.Setenv("TAILTERM_ACTIVITY_PROVIDER_REPEAT", value)
		if activityDefaults().ProviderRepeat != 2 {
			t.Fatalf("override %q not bounded", value)
		}
	}
}

func TestActivityProviderBlockedTick(t *testing.T) {
	cols, rows := 200, 50
	stubPaneSize(t, &cols, &rows)
	lines := strings.Split(strings.TrimSpace(providerFixture(t, "claude-fable-limit")), "\n")
	write := func(t *testing.T, b runtimeBinding, records ...string) {
		t.Helper()
		path := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "p", b.Thread+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 9, 26, 19, 0, 0, 0, time.UTC)
	recovery := `{"type":"assistant","timestamp":"2026-09-26T20:21:00.000Z","message":{"model":"claude-opus-5-5","id":"msg_fixture_recovered","role":"assistant","content":[{"type":"text","text":"Synthetic fixture reply."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}`

	t.Run("one report per transition", func(t *testing.T) {
		b, h, _, tick := stuckFixture(t, "claude")
		write(t, b, lines[:5]...)
		if err := tick(now); err != nil {
			t.Fatal(err)
		}
		if len(h.reports) != 1 || h.reports[0].State != "provider_blocked" || h.reports[0].Provider.Class != "usage_limit" {
			t.Fatalf("entering %+v", h.reports)
		}
		// More failed prompts: the block is steady and is not reported again.
		write(t, b, lines...)
		for i := 1; i <= 3; i++ {
			if err := tick(now.Add(time.Duration(i) * 16 * time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		if len(h.reports) != 1 {
			t.Fatalf("steady block reported again %+v", h.reports)
		}
		// A tiny pane does not hide the block behind stuck.
		cols, rows = 16, 1
		if err := tick(now.Add(80 * time.Second)); err != nil || len(h.reports) != 1 {
			t.Fatalf("stuck replaced the block: %v %+v", err, h.reports)
		}
		cols, rows = 200, 50
		write(t, b, append(append([]string{}, lines...), recovery)...)
		if err := tick(now.Add(100 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if len(h.reports) != 2 || h.reports[1].State != "idle" || h.reports[1].Provider != nil || h.reports[1].Reason != "" {
			t.Fatalf("leaving %+v", h.reports)
		}
	})

	// A real test hub: it stores provider_blocked, and a runtime prompt on the
	// agent's pane then takes precedence.
	t.Run("runtime prompt overrides", func(t *testing.T) {
		r := newPromptRig(t, "claude")
		path := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "rig", r.b.Thread+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		got := r.tick()
		if got.State != "provider_blocked" || got.Provider == nil || got.Provider.Class != "usage_limit" || got.Provider.Model != "claude-fable-5-1" {
			t.Fatalf("hub snapshot %+v %+v", got, got.Provider)
		}
		r.screen = r.fixture("claude-pane/permission-dialog.ansi")
		if got = r.tick(); got.State != "runtime_prompt" || got.Prompt == nil || got.Provider != nil {
			t.Fatalf("runtime prompt did not override %+v", got)
		}
		r.screen = ""
		if got = r.tick(); got.State != "provider_blocked" || got.Provider == nil || got.Prompt != nil {
			t.Fatalf("block not restored after the prompt %+v", got)
		}
	})

	t.Run("older hub gets the legacy form", func(t *testing.T) {
		for _, runtime := range []string{"claude", "codex"} {
			b, h, _, tick := stuckFixture(t, runtime)
			h.rejectState = "provider_blocked"
			wantState, wantReason := "idle", "turn ended by API error (rate_limit)"
			at := now
			if runtime == "claude" {
				write(t, b, lines...)
			} else {
				path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "09", "29", "rollout-test-"+b.Thread+".jsonl")
				if err := os.WriteFile(path, []byte(providerFixture(t, "codex-401")), 0600); err != nil {
					t.Fatal(err)
				}
				wantState, wantReason = "unknown", "provider_blocked: auth"
			}
			// The 400 is remembered; the next tick sends the legacy form.
			if err := tick(at); err != nil || len(h.reports) != 0 {
				t.Fatalf("%s rejected tick %v %+v", runtime, err, h.reports)
			}
			if err := tick(at.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if len(h.reports) != 1 || h.reports[0].State != wantState || h.reports[0].Reason != wantReason || h.reports[0].Provider != nil {
				t.Fatalf("%s legacy form %+v", runtime, h.reports)
			}
			if err := tick(at.Add(20 * time.Second)); err != nil || len(h.reports) != 1 || h.rejected != 1 {
				t.Fatalf("%s repeat %v reports=%+v rejected=%d", runtime, err, h.reports, h.rejected)
			}
			// Later transitions still report.
			if runtime == "claude" {
				write(t, b, append(append([]string{}, lines...), recovery)...)
				if err := tick(at.Add(40 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if len(h.reports) != 2 || h.reports[1].State != "idle" || h.reports[1].Reason != "" {
					t.Fatalf("claude later transition %+v", h.reports)
				}
			} else {
				cols, rows = 16, 1
				if err := tick(at.Add(40 * time.Second)); err != nil {
					t.Fatal(err)
				}
				cols, rows = 200, 50
				if len(h.reports) != 2 || h.reports[1].State != "stuck" {
					t.Fatalf("codex later transition %+v", h.reports)
				}
			}
		}
	})

	if activityReportKey(api.AgentActivity{State: "provider_blocked", Provider: &api.ProviderBlock{Class: "auth", Since: now}}) == activityReportKey(api.AgentActivity{State: "provider_blocked", Provider: &api.ProviderBlock{Class: "server_error", Since: now}}) {
		t.Fatal("a class change must be a new report")
	}
}

func TestActivityCLIShowsProviderBlocked(t *testing.T) {
	task := "tsk_0123456789abcdef"
	blocked := &api.AgentActivity{State: "provider_blocked", ObservedAt: time.Date(2026, 9, 26, 19, 0, 0, 0, time.UTC),
		Provider: &api.ProviderBlock{Provider: "anthropic", Runtime: "claude", Model: "claude-fable-5-1", Class: "usage_limit", Code: "rate_limit", Status: 429, Since: time.Date(2026, 9, 26, 18, 53, 10, 0, time.UTC)}}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/agents") {
			_ = json.NewEncoder(w).Encode(api.AgentList{Agents: []api.Agent{{ID: "agt_0123456789abcdef", TaskID: task, Name: "reviewer", Session: "fake", Host: "mini", Status: api.AgentRunning, Activity: blocked}}})
		} else if strings.HasSuffix(r.URL.Path, "/team-queue") {
			_ = json.NewEncoder(w).Encode(api.TeamQueueList{ConcurrencyLimit: 1, Entries: []api.TeamQueueEntry{{ID: "tqe_0123456789abcdef", TaskID: task, ItemID: "wi_0123456789abcdef", State: "running",
				Activities: []api.TeamAgentActivity{{Name: "reviewer", Activity: blocked}, {Name: "builder", Activity: &api.AgentActivity{State: "working"}}, {Name: "planner"}}}}})
		} else {
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	queue, err := captureRelayOutput(t, false, func() error { return cmdTeamQueue(env{hub: hub.URL, task: task}, []string{"list"}) })
	if err != nil || !strings.Contains(queue, "  reviewer activity=provider_blocked provider=anthropic class=usage_limit\n") || !strings.Contains(queue, "  builder activity=working\n") || !strings.Contains(queue, "  planner activity=unknown\n") {
		t.Fatalf("queue output %q %v", queue, err)
	}
	agents, err := captureRelayOutput(t, false, func() error { return cmdAgents(env{hub: hub.URL, task: task}, nil) })
	if err != nil || !strings.Contains(agents, "activity=provider_blocked") || !strings.Contains(agents, "  provider-blocked provider=anthropic model=claude-fable-5-1 class=usage_limit status=429 since=2026-09-26T18:53:10Z\n") {
		t.Fatalf("agents output %q %v", agents, err)
	}
	if got := queueMemberActivity(blocked); got != "activity=provider_blocked provider=anthropic class=usage_limit" {
		t.Fatalf("queue member %q", got)
	}
	if got := queueMemberActivity(&api.AgentActivity{State: "stuck"}); got != "activity=stuck" {
		t.Fatalf("queue member stuck %q", got)
	}
	if got := queueMemberActivity(nil); got != "activity=unknown" {
		t.Fatalf("queue member nil %q", got)
	}
}

func TestOwnerHelperCodexDiscoveryHeartbeatAndOffline(t *testing.T) {
	f := newHelperFixture(t)
	helperTestRuntime(t, f, "codex", f.thread)
	processDir := t.TempDir()
	if err := os.Symlink("/bin/sleep", filepath.Join(processDir, "codex")); err != nil {
		t.Fatal(err)
	}
	f.tmux(t, "new-session", "-d", "-s", "codex-owner", filepath.Join(processDir, "codex")+" 300")
	t.Setenv("TMUX_PANE", f.tmux(t, "display-message", "-p", "-t", "codex-owner:", "#{pane_id}"))
	a := *f.mustRegister(t).Agent
	b, _ := readBinding(t, f.owner.hub, a.ID)
	receipt, err := nativeRuntimeDiscovery(context.Background(), b, a)
	if err != nil || receipt.Run != a.RunID || receipt.Session != "codex-owner" || receipt.PID <= 0 {
		t.Fatalf("Codex discovery %+v %v", receipt, err)
	}
	probe := func(got runtimeBinding, agent api.Agent) (bool, bool, error) {
		_, err := nativeRuntimeDiscovery(context.Background(), got, agent)
		return err == nil, err == nil, err
	}
	now := time.Now()
	var p relayProgress
	if err := relayHelperHeartbeat(context.Background(), b, &p, f.c, now, probe); err != nil {
		t.Fatal(err)
	}
	current, err := f.c.GetAgent(context.Background(), f.task.ID, a.ID)
	if err != nil || !current.Online {
		t.Fatalf("helper heartbeat %+v %v", current, err)
	}
	previous := current.LastSeenAt
	f.tmux(t, "kill-session", "-t", "codex-owner")
	if err := relayHelperHeartbeat(context.Background(), b, &p, f.c, now.Add(31*time.Second), probe); err != nil {
		t.Fatal(err)
	}
	current, err = f.c.GetAgent(context.Background(), f.task.ID, a.ID)
	if err != nil || !current.LastSeenAt.Equal(previous) {
		t.Fatalf("missing Codex process heartbeated %+v %v", current, err)
	}
	var cursor activityCursor
	got := activityState(&cursor, a, 1, false, false, nil, now.Add(2*time.Minute), activityDefaults())
	if got.State != "unknown" || got.Reason != "owner session offline" {
		t.Fatalf("Codex helper absence %+v", got)
	}
}
