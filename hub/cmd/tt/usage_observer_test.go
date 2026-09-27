package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSyntheticUsage(t *testing.T, runtime string) *usageCursor {
	t.Helper()
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	u, err := loadUsageCursor(runtimeBinding{Hub: "http://fixture.invalid", Task: "tsk_1111111111111111", Agent: "agt_1111111111111111", Run: "run_1111111111111111", Thread: "12345678-1234-1234-1234-123456789abc", Runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func parseUsageLines(t *testing.T, u *usageCursor, lines ...string) {
	t.Helper()
	for _, s := range lines {
		if err := u.parse([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
}
func codexUsageEvent(at string, last, total map[string]int64) string {
	raw, _ := json.Marshal(map[string]any{"timestamp": at, "type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"last_token_usage": last, "total_token_usage": total}}})
	return string(raw)
}
func TestUsageCodexRequestsHistoricalModelsSubsetAndResets(t *testing.T) {
	u := newSyntheticUsage(t, "codex")
	start := `{"timestamp":"2026-09-27T10:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"activation"}}`
	one := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "cache_write_tokens": 0, "output_tokens": 10, "reasoning_output_tokens": 4, "total_tokens": 110}
	parseUsageLines(t, u, start, `{"timestamp":"2026-09-27T10:00:00Z","type":"turn_context","payload":{"model":"model-one"}}`, codexUsageEvent("2026-09-27T10:00:01Z", one, one), codexUsageEvent("2026-09-27T10:00:02Z", one, one))
	if len(u.Turns) != 1 {
		t.Fatal("duplicate snapshot", u)
	}
	for _, x := range u.Turns {
		if x.Tokens["input"] != 20 || x.Tokens["cached"] != 80 || x.Tokens["output"] != 6 || x.Model != "model-one" {
			t.Fatal(x)
		}
	}
	total := map[string]int64{}
	for k, v := range one {
		total[k] = v * 2
	}
	parseUsageLines(t, u, `{"timestamp":"2026-09-27T10:00:03Z","type":"turn_context","payload":{"model":"model-two"}}`, codexUsageEvent("2026-09-27T10:00:04Z", one, total), `{"timestamp":"2026-09-27T10:00:05Z","type":"event_msg","payload":{"type":"task_complete"}}`)
	if len(u.Turns) != 2 {
		t.Fatal("request count", len(u.Turns))
	}
	models := map[string]bool{}
	for _, x := range u.Turns {
		models[x.Model] = true
		if !x.Complete || x.Revision != 1 {
			t.Fatal("unuploaded final revision", x)
		}
	}
	if !models["model-one"] || !models["model-two"] {
		t.Fatal(models)
	}
	parseUsageLines(t, u, codexUsageEvent("2026-09-27T10:00:06Z", one, one))
	if len(u.Turns) != 2 || !strings.Contains(u.Coverage, "reset") {
		t.Fatal("reset fabricated", u)
	}
	v := newSyntheticUsage(t, "codex")
	parseUsageLines(t, v, start, codexUsageEvent("2026-09-27T10:00:01Z", one, total))
	if len(v.Turns) != 0 || !strings.Contains(v.Coverage, "history") {
		t.Fatal("lifetime counted as first request", v)
	}
	tokens, gap := normalizeUsage("codex", map[string]int64{"input_tokens": 100, "output_tokens": 10})
	if _, ok := tokens["cached"]; ok || gap == "" {
		t.Fatal("missing cached became zero", tokens, gap)
	}
}
func TestUsageClaudeStreamingOneRequestAndClassAvailability(t *testing.T) {
	u := newSyntheticUsage(t, "claude")
	parseUsageLines(t, u, `{"type":"user","timestamp":"2026-09-27T10:00:00Z","message":{"content":"synthetic"}}`, `{"type":"assistant","timestamp":"2026-09-27T10:00:01Z","message":{"id":"msg-one","model":"model-one","usage":{"input_tokens":3,"cache_read_input_tokens":80,"cache_creation_input_tokens":12,"output_tokens":1}}}`, `{"type":"assistant","timestamp":"2026-09-27T10:00:02Z","message":{"id":"msg-one","model":"model-one","usage":{"input_tokens":3,"cache_read_input_tokens":80,"cache_creation_input_tokens":12,"output_tokens":7}}}`, `{"type":"assistant","timestamp":"2026-09-27T10:00:03Z","message":{"id":"msg-one","model":"model-one","stop_reason":"end_turn","usage":{"input_tokens":3,"cache_read_input_tokens":80,"cache_creation_input_tokens":12,"output_tokens":7}}}`)
	if len(u.Turns) != 1 {
		t.Fatal(len(u.Turns))
	}
	x := u.Turns["claude-msg-one"]
	if x.Revision != 1 || x.Tokens["input"] != 3 || x.Tokens["cached"] != 80 || x.Tokens["cacheWrite"] != 12 || x.Tokens["output"] != 7 || !x.Complete {
		t.Fatal(x)
	}
	if _, ok := x.Tokens["reasoning"]; ok {
		t.Fatal("unsupported reasoning inferred", x)
	}
}
func TestUsageOpeningRequestGetsHandledActivationAndNextDoesNotInherit(t *testing.T) {
	u := newSyntheticUsage(t, "codex")
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	entry := api.UsageEvidence{TaskID: u.Binding.Task, Seq: 4, Operation: "ack", At: at.Add(2 * time.Second)}
	if err := writePrivateJSON(usageContextPath(env{agent: u.Binding.Agent, runID: u.Binding.Run}), []api.UsageEvidence{entry, {TaskID: u.Binding.Task, Seq: 5, Operation: "inbox", At: at.Add(time.Second)}}); err != nil {
		t.Fatal(err)
	}
	one := map[string]int64{"input_tokens": 10, "cached_input_tokens": 0, "output_tokens": 1, "reasoning_output_tokens": 0}
	parseUsageLines(t, u, `{"type":"task_started","timestamp":"2026-09-27T10:00:00Z"}`, codexUsageEvent("2026-09-27T10:00:01Z", one, one), `{"type":"task_complete","timestamp":"2026-09-27T10:00:03Z"}`)
	for _, x := range u.Turns {
		if len(x.Handled) != 1 || x.Handled[0].Seq != 4 {
			t.Fatal("opening input omitted or inbox credited", x)
		}
	}
	total := map[string]int64{}
	for k, v := range one {
		total[k] = v * 2
	}
	parseUsageLines(t, u, `{"type":"task_started","timestamp":"2026-09-27T10:00:04Z"}`, codexUsageEvent("2026-09-27T10:00:05Z", one, total), `{"type":"task_complete","timestamp":"2026-09-27T10:00:06Z"}`)
	for _, x := range u.Turns {
		if x.At.After(at.Add(4*time.Second)) && len(x.Handled) != 0 {
			t.Fatal("prior activation inherited", x)
		}
	}
}
func TestUsageOutboxLostReplyRestartAndArchivedNoPolls(t *testing.T) {
	u := newSyntheticUsage(t, "claude")
	parseUsageLines(t, u, `{"type":"assistant","timestamp":"2026-09-27T10:00:01Z","message":{"id":"one","model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}}`)
	if err := freezeUsageBatch(u); err != nil {
		t.Fatal(err)
	}
	key := u.Pending.RequestID
	var keys []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/usage") {
			t.Error("agent poll or wrong method", r.URL)
			w.WriteHeader(500)
			return
		}
		var b api.UsageBatch
		_ = json.NewDecoder(r.Body).Decode(&b)
		keys = append(keys, b.RequestID)
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(api.UsageReceipt{RequestID: b.RequestID, Turns: len(b.Turns)})
	}))
	defer srv.Close()
	c, _ := api.NewClient(srv.URL, time.Second)
	if err := uploadUsage(context.Background(), u, c); err == nil {
		t.Fatal("uncertain storage reported success")
	}
	restart, err := loadUsageCursor(u.Binding)
	if err != nil || restart.Pending.RequestID != key {
		t.Fatal(restart, err)
	}
	restart.LastUploadAttempt = time.Time{}
	if err = saveUsageCursor(restart); err != nil {
		t.Fatal(err)
	}
	if err = freezeRetiredUsage(u.Binding); err != nil {
		t.Fatal(err)
	}
	if err = flushFrozenUsage(context.Background(), relayDir(), func(runtimeBinding) (*api.Client, error) { return c, nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatal("retry identity changed", keys)
	}
	// Receipt persisted before clearing immutable outbox; another host pass has no upload.
	if err = flushFrozenUsage(context.Background(), relayDir(), func(runtimeBinding) (*api.Client, error) { return c, nil }); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
}
func TestUsageBoundedAppendPartialAndNoTranscriptText(t *testing.T) {
	u := newSyntheticUsage(t, "claude")
	path := filepath.Join(t.TempDir(), "synthetic.jsonl")
	line := `{"type":"assistant","timestamp":"2026-09-27T10:00:01Z","message":{"id":"one","model":"m","content":[{"type":"text","text":"PRIVATE_SYNTHETIC_TEXT"}],"usage":{"input_tokens":1,"output_tokens":2}}}`
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	var cursor activityCursor
	parse := func(b []byte, c *activityCursor) error { return u.parse(b) }
	if err := readActivityAppend(path, &cursor, parse); err != nil || len(u.Turns) != 0 {
		t.Fatal("partial emitted", err)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	fmt.Fprintln(f)
	f.Close()
	if err := readActivityAppend(path, &cursor, parse); err != nil || len(u.Turns) != 1 {
		t.Fatal(err, len(u.Turns))
	}
	if err := freezeUsageBatch(u); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(u.Pending)
	if strings.Contains(string(raw), "PRIVATE_SYNTHETIC_TEXT") || strings.Contains(string(raw), "content") {
		t.Fatal("transcript content escaped", string(raw))
	}
	many := strings.Repeat("{}\n", 1000)
	os.WriteFile(path, []byte(many), 0600)
	cursor = activityCursor{}
	count := 0
	readActivityAppend(path, &cursor, func([]byte, *activityCursor) error { count++; return nil })
	if count > 256 || cursor.Ready {
		t.Fatal("unbounded pass", count, cursor)
	}
}

func TestUsageLateClaudeOutputUsesNextProjectionNotAnotherRequest(t *testing.T) {
	u := newSyntheticUsage(t, "claude")
	parseUsageLines(t, u, `{"type":"assistant","timestamp":"2026-09-27T10:00:01Z","message":{"id":"late","model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}}`)
	first := u.Turns["claude-late"]
	u.Uploaded[first.ID] = 1
	delete(u.Dirty, first.ID)
	parseUsageLines(t, u, `{"type":"assistant","timestamp":"2026-09-27T10:00:02Z","message":{"id":"late","model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":8}}}`)
	next := u.Turns[first.ID]
	if len(u.Turns) != 1 || next.Revision != 2 || next.Tokens["output"] != 8 || !next.At.Equal(first.At) || !next.Complete {
		t.Fatal(next)
	}
}

func TestUsageCapabilityGatesLegacyHubWithoutExtraAgentPolls(t *testing.T) {
	u := newSyntheticUsage(t, "codex")
	gets := 0
	supported := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capabilities" {
			t.Error("unexpected poll", r.URL)
		}
		gets++
		caps := api.CurrentCapabilities()
		caps.Usage.Supported = supported
		json.NewEncoder(w).Encode(caps)
	}))
	defer srv.Close()
	c, _ := api.NewClient(srv.URL, time.Second)
	u.Binding.Hub = srv.URL
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		ctx := prepareUsageContext(context.Background(), u.Binding, c, now.Add(time.Duration(i)*time.Second))
		if enabled, _ := ctx.Value(usageEnabledContextKey{}).(bool); !enabled {
			t.Fatal("supported hub not enabled")
		}
	}
	if gets != 1 {
		t.Fatal("capability checks unbounded", gets)
	}
	supported = false
	ctx := prepareUsageContext(context.Background(), u.Binding, c, now.Add(6*time.Minute))
	if enabled, _ := ctx.Value(usageEnabledContextKey{}).(bool); enabled {
		t.Fatal("legacy hub enabled")
	}
	if gets != 2 {
		t.Fatal(gets)
	}
}
func TestUsageSessionEpochMismatchNeverEmitsTokens(t *testing.T) {
	u := newSyntheticUsage(t, "codex")
	raw := map[string]int64{"input_tokens": 10, "cached_input_tokens": 0, "output_tokens": 1, "reasoning_output_tokens": 0}
	parseUsageLines(t, u, `{"type":"session_meta","timestamp":"2026-09-27T10:00:00Z","payload":{"id":"another-session"}}`, codexUsageEvent("2026-09-27T10:00:01Z", raw, raw))
	if len(u.Turns) != 0 || !u.WrongEpoch {
		t.Fatal(u)
	}
}

func TestUsageRelayAppendThroughRealHTTPStore(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "synthetic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic relay usage"}, by)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "synthetic-handler", AgentID: api.NewID("agt"), Host: "fixture", Session: "fixture", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, server.StaticIdentity(by)))
	defer srv.Close()
	client, err := api.NewClient(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: agent.ID, Run: agent.RunID, Thread: "12345678-1234-1234-1234-123456789abc", Runtime: "codex", CodexHome: t.TempDir()}
	dir := filepath.Join(b.CodexHome, "sessions", "2026", "09", "27")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "output_tokens": 10, "reasoning_output_tokens": 4}
	content := strings.Join([]string{
		`{"timestamp":"2026-09-27T10:00:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"synthetic"}}`,
		`{"timestamp":"2026-09-27T10:00:00Z","type":"turn_context","payload":{"model":"synthetic-model"}}`,
		codexUsageEvent("2026-09-27T10:00:01Z", raw, raw),
		`{"timestamp":"2026-09-27T10:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n") + "\n"
	if err = os.WriteFile(filepath.Join(dir, "rollout-fixture-"+b.Thread+".jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx = context.WithValue(ctx, usageEnabledContextKey{}, true)
	now := time.Now().UTC()
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	if err = relayActivityTick(ctx, b, client, now, probe); err != nil {
		t.Fatal(err)
	}
	u, err := loadUsageCursor(b)
	if err != nil {
		t.Fatal(err)
	}
	u.LastUploadAttempt = time.Time{}
	if err = saveUsageCursor(u); err != nil {
		t.Fatal(err)
	}
	if err = relayActivityTick(ctx, b, client, now.Add(16*time.Second), probe); err != nil {
		t.Fatal(err)
	}
	report, err := client.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Requests != 1 || report.Overhead.Summary.Requests != 1 || report.Summary.Tokens["input"] != "20" || report.Summary.Tokens["cached"] != "80" {
		t.Fatalf("real relay/store report: %+v", report)
	}
	// Repeated append passes and hub reads do not manufacture another request.
	if err = relayActivityTick(ctx, b, client, now.Add(32*time.Second), probe); err != nil {
		t.Fatal(err)
	}
	report, err = client.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil || report.Summary.Requests != 1 {
		t.Fatal(report, err)
	}
}

func TestUsageHandledRetentionGapIsExplicit(t *testing.T) {
	u := newSyntheticUsage(t, "codex")
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	entries := make([]api.UsageEvidence, 65)
	for i := range entries {
		entries[i] = api.UsageEvidence{TaskID: u.Binding.Task, Seq: int64(i + 1), Operation: "ack", At: at.Add(time.Second)}
	}
	if err := writePrivateJSON(usageContextPath(env{agent: u.Binding.Agent, runID: u.Binding.Run}), entries); err != nil {
		t.Fatal(err)
	}
	parseUsageLines(t, u, `{"timestamp":"2026-09-27T10:00:00Z","type":"event_msg","payload":{"type":"task_started"}}`, codexUsageEvent("2026-09-27T10:00:01Z", map[string]int64{"input_tokens": 1, "cached_input_tokens": 0}, map[string]int64{"input_tokens": 1, "cached_input_tokens": 0}), `{"timestamp":"2026-09-27T10:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}`)
	if !strings.Contains(u.Coverage, "retention") {
		t.Fatal(u.Coverage)
	}
	for _, turn := range u.Turns {
		if len(turn.Handled) != 64 {
			t.Fatal(turn)
		}
	}
}
