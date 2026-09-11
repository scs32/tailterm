package monitor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

func fixture(t *testing.T) (*store.Store, string, api.Task, api.Agent, *Enforcer, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monitor.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Monitor", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.StallAfter, config.WorkerSilence = time.Minute, time.Minute
	config.InitialBackoff, config.MaxBackoff = 5*time.Minute, time.Hour
	e, err := New(st, config)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	return st, path, task, lead, e, &now
}

func TestDetectStallMatrix(t *testing.T) {
	_, _, _, _, e, now := fixture(t)
	entry := api.QueueEntry{ID: "que_1111111111111111", Cycle: 1, Revision: 3, State: api.QueueStateWaiting, Eligible: true, UpdatedAt: now.Add(-time.Minute)}
	got, intentional, unknown := e.detect([]api.QueueEntry{entry}, nil, *now)
	if got == "" || intentional || unknown {
		t.Fatalf("waiting detection = %q intentional=%v unknown=%v", got, intentional, unknown)
	}
	entry.State, entry.WorkerAgentID, entry.WorkerRunID = api.QueueStateActive, "agt_1111111111111111", "run_1111111111111111"
	entry.Revision = 4
	entry.UpdatedAt = now.Add(-time.Minute)
	worker := api.Agent{ID: entry.WorkerAgentID, RunID: entry.WorkerRunID, Status: api.AgentRunning, LastSeenAt: *now}
	got, intentional, unknown = e.detect([]api.QueueEntry{entry}, map[string]api.Agent{worker.ID: worker}, *now)
	if got == "" || intentional || unknown {
		t.Fatalf("healthy-heartbeat stale-work detection = %q intentional=%v unknown=%v", got, intentional, unknown)
	}
	if got[:12] != "active_stale" {
		t.Fatalf("healthy heartbeat hid stale Queue evidence: %q", got)
	}
	worker.LastSeenAt = now.Add(-time.Minute)
	got, intentional, unknown = e.detect([]api.QueueEntry{entry}, map[string]api.Agent{worker.ID: worker}, *now)
	if got == "" || intentional || unknown {
		t.Fatalf("silent worker detection = %q intentional=%v unknown=%v", got, intentional, unknown)
	}
	worker.Status = api.AgentNeedsInput
	got, intentional, unknown = e.detect([]api.QueueEntry{entry}, map[string]api.Agent{worker.ID: worker}, *now)
	if got != "" || !intentional || unknown {
		t.Fatalf("blocked worker = %q intentional=%v unknown=%v", got, intentional, unknown)
	}
	worker.Status, worker.LastSeenAt = api.AgentRunning, time.Time{}
	got, intentional, unknown = e.detect([]api.QueueEntry{entry}, map[string]api.Agent{worker.ID: worker}, *now)
	if got != "" || intentional || !unknown {
		t.Fatalf("unknown worker = %q intentional=%v unknown=%v", got, intentional, unknown)
	}
	blocked := entry
	blocked.ID, blocked.Revision = "que_2222222222222222", 5
	worker.Status, worker.LastSeenAt = api.AgentNeedsInput, *now
	got, intentional, unknown = e.detect([]api.QueueEntry{blocked, api.QueueEntry{ID: "que_3333333333333333", Cycle: 1, Revision: 1, State: api.QueueStateWaiting, Eligible: true, UpdatedAt: now.Add(-time.Minute)}}, map[string]api.Agent{worker.ID: worker}, *now)
	if got == "" || intentional || unknown {
		t.Fatalf("blocked entry hid independent waiting work = %q intentional=%v unknown=%v", got, intentional, unknown)
	}
	unknownEntry := blocked
	unknownEntry.ID = "que_4444444444444444"
	unknownWorker := worker
	unknownWorker.Status, unknownWorker.LastSeenAt = api.AgentRunning, time.Time{}
	got, intentional, unknown = e.detect([]api.QueueEntry{unknownEntry, api.QueueEntry{ID: "que_5555555555555555", Cycle: 1, Revision: 1, State: api.QueueStateWaiting, Eligible: true, UpdatedAt: now.Add(-time.Minute)}}, map[string]api.Agent{unknownWorker.ID: unknownWorker}, *now)
	if got == "" || intentional || unknown {
		t.Fatalf("unknown entry hid independent waiting work = %q intentional=%v unknown=%v", got, intentional, unknown)
	}
}

func TestDeliveryBackoffAndRecoveryAreDurable(t *testing.T) {
	st, path, task, lead, e, now := fixture(t)
	var outcomes []Outcome
	report := func(out Outcome) { outcomes = append(outcomes, out) }
	fingerprint := "waiting:que_1111111111111111:1:3:"
	e.deliver(context.Background(), task.ID, lead, fingerprint, *now, report)
	if len(outcomes) != 1 || outcomes[0].State != "notified" || outcomes[0].MessageSeq == 0 {
		t.Fatalf("first delivery: %+v", outcomes)
	}
	first := outcomes[0].MessageSeq
	e.deliver(context.Background(), task.ID, lead, fingerprint, now.Add(time.Minute), report)
	if outcomes[1].State != "suppressed" || outcomes[1].MessageSeq != first {
		t.Fatalf("backoff result: %+v", outcomes[1])
	}
	e.deliver(context.Background(), task.ID, lead, fingerprint, now.Add(5*time.Minute), report)
	if outcomes[2].State != "notified" || outcomes[2].MessageSeq == first {
		t.Fatalf("retry delivery: %+v", outcomes[2])
	}
	messages, err := st.ListMessages(context.Background(), task.ID, 0, lead.ID, 10)
	if err != nil || len(messages) != 2 || messages[0].Text[:3] != "GO!" || messages[0].To != lead.ID {
		t.Fatalf("directed messages=%+v err=%v", messages, err)
	}
	if got, err := st.GetAgent(context.Background(), lead.ID); err != nil || got.Status != api.AgentStarting {
		t.Fatalf("notification mutated lead lifecycle: %+v %v", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := New(reopened, e.config)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now.Add(6 * time.Minute) }
	restartedOutcomes := []Outcome{}
	restarted.deliver(context.Background(), task.ID, lead, fingerprint, now.Add(6*time.Minute), func(out Outcome) { restartedOutcomes = append(restartedOutcomes, out) })
	if len(restartedOutcomes) != 1 || restartedOutcomes[0].State != "suppressed" {
		t.Fatalf("recovery dedup: %+v", restartedOutcomes)
	}
}

func TestTickSkipsNoWorkAndUnknownLead(t *testing.T) {
	_, _, task, _, e, _ := fixture(t)
	var outcomes []Outcome
	e.Tick(context.Background(), func(out Outcome) { outcomes = append(outcomes, out) })
	if len(outcomes) != 1 || outcomes[0].TaskID != task.ID || outcomes[0].State != "no_work" {
		t.Fatalf("no-work outcomes: %+v", outcomes)
	}
}

func TestDeliveryFailureIsRecordedWithoutLifecycleMutation(t *testing.T) {
	st, _, task, lead, e, now := fixture(t)
	if _, err := st.CloseTask(context.Background(), task.ID, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	var outcomes []Outcome
	e.deliver(context.Background(), task.ID, lead, "waiting:que_1111111111111111:1:3:", *now, func(out Outcome) { outcomes = append(outcomes, out) })
	if len(outcomes) != 1 || outcomes[0].State != "delivery_failed" {
		t.Fatalf("closed-task delivery: %+v", outcomes)
	}
	if got, err := st.GetAgent(context.Background(), lead.ID); err != nil || got.Status != api.AgentClosed {
		t.Fatalf("failed delivery changed lifecycle: %+v %v", got, err)
	}
	e.deliver(context.Background(), task.ID, lead, "waiting:que_1111111111111111:1:3:", now.Add(time.Minute), func(out Outcome) { outcomes = append(outcomes, out) })
	if len(outcomes) != 2 || outcomes[1].State != "suppressed" {
		t.Fatalf("failed delivery did not back off: %+v", outcomes)
	}
	notice, err := st.GetScheduleMonitorNotice(context.Background(), task.ID, "waiting:que_1111111111111111:1:3:")
	if err != nil || notice.FailureCount != 1 || notice.LastAttemptAt != *now {
		t.Fatalf("failed delivery state: %+v %v", notice, err)
	}
}

func TestTickPrunesExpiredNoticeMetadata(t *testing.T) {
	st, _, task, _, e, now := fixture(t)
	fingerprint := "waiting:que_1111111111111111:1:3:"
	old := now.Add(-8 * 24 * time.Hour)
	if err := st.SaveScheduleMonitorNotice(context.Background(), store.ScheduleMonitorNotice{TaskID: task.ID, Fingerprint: fingerprint, FirstDetectedAt: old, LastObservedAt: old}); err != nil {
		t.Fatal(err)
	}
	e.Tick(context.Background(), func(Outcome) {})
	if _, err := st.GetScheduleMonitorNotice(context.Background(), task.ID, fingerprint); err != api.ErrNotFound {
		t.Fatalf("expired notice retained: %v", err)
	}
}
