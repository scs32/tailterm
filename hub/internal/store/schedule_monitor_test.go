package store

import (
	"context"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type monitorFixture struct {
	s    *Store
	path string
	task api.Task
	lead api.Agent
	now  time.Time
}

var monitorCaller = api.Caller{Node: "fixture", User: "owner"}

const monitorFingerprint = "waiting:que_1111111111111111:1:3:"

func newMonitorFixture(t *testing.T) *monitorFixture {
	t.Helper()
	f := &monitorFixture{path: filepath.Join(t.TempDir(), "monitor.sqlite"), now: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
	var err error
	f.s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.s.Close() })
	f.s.now = func() time.Time { return f.now }
	f.task, err = f.s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Synthetic Monitor", Orchestrator: "lead"}, monitorCaller)
	if err != nil {
		t.Fatal(err)
	}
	f.lead, err = f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, monitorCaller)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *monitorFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.s.now = func() time.Time { return f.now }
}
func (f *monitorFixture) deliver(lead api.Agent) (ScheduleMonitorNotice, string, error) {
	return f.s.ScheduleMonitorDelivery(context.Background(), f.task.ID, lead, monitorFingerprint, "GO! synthetic notification", f.now, time.Minute, 8*time.Minute)
}
func (f *monitorFixture) messages(t *testing.T) []api.Message {
	t.Helper()
	m, e := f.s.ListMessages(context.Background(), f.task.ID, 0, "", 100)
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func (f *monitorFixture) sql(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.s.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *monitorFixture) status(t *testing.T, status string) {
	t.Helper()
	if _, err := f.s.UpdateAgent(context.Background(), f.lead.ID, api.UpdateAgentRequest{Status: &status}, monitorCaller); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleMonitorRetirementInterleavingPreservesHumanResume(t *testing.T) {
	f := newMonitorFixture(t)
	snapshot := f.lead // Snapshot precedes explicit retirement.
	f.sql(t, `UPDATE agents SET last_seen_at=? WHERE id=?`, ts(time.Now().UTC()), snapshot.ID)
	f.status(t, api.AgentRetired)
	before, err := f.s.GetAgent(context.Background(), snapshot.ID)
	if err != nil || !before.Online {
		t.Fatalf("fixture %+v %v", before, err)
	}
	_, state, err := f.deliver(snapshot)
	if err != nil || state != "intentional_block" {
		t.Fatalf("delivery %s %v", state, err)
	}
	after, _ := f.s.GetAgent(context.Background(), snapshot.ID)
	if !reflect.DeepEqual(before, after) || len(f.messages(t)) != 0 {
		t.Fatalf("retired run changed: %+v => %+v", before, after)
	}
	events, err := f.s.ListEvents(context.Background(), f.task.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == api.EventResumed || event.Kind == api.EventMessage {
			t.Fatalf("unexpected event %+v", event)
		}
	}
	if _, err = f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{To: snapshot.ID, Text: "Please continue", RequestID: "explicit-human-resume"}, monitorCaller); err != nil {
		t.Fatal(err)
	}
	after, _ = f.s.GetAgent(context.Background(), snapshot.ID)
	if after.Status != api.AgentDone || after.RunID != snapshot.RunID {
		t.Fatalf("human resume %+v", after)
	}
}
func TestScheduleMonitorRecipientRecheckMatrix(t *testing.T) {
	for _, kind := range []string{"needs_input", "closed", "exited", "run_replaced", "lead_changed", "offline"} {
		t.Run(kind, func(t *testing.T) {
			f := newMonitorFixture(t)
			snapshot := f.lead
			want := "intentional_block"
			switch kind {
			case "needs_input", "closed", "exited":
				f.status(t, kind)
			case "run_replaced":
				f.sql(t, `UPDATE agents SET run_id='run_2222222222222222' WHERE id=?`, snapshot.ID)
				want = "unknown"
			case "lead_changed":
				f.sql(t, `UPDATE tasks SET orchestrator='replacement' WHERE id=?`, f.task.ID)
				want = "unknown"
			case "offline":
				f.sql(t, `UPDATE agents SET last_seen_at='' WHERE id=?`, snapshot.ID)
				want = "notified"
			}
			before, _ := f.s.GetAgent(context.Background(), snapshot.ID)
			_, state, err := f.deliver(snapshot)
			if state != want || (want != "unknown" && err != nil) {
				t.Fatalf("%s %v", state, err)
			}
			after, _ := f.s.GetAgent(context.Background(), snapshot.ID)
			before.Unread, after.Unread = 0, 0
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("lifecycle changed: %+v => %+v", before, after)
			}
			count := 0
			if want == "notified" {
				count = 1
			}
			if len(f.messages(t)) != count {
				t.Fatal("unexpected mail")
			}
		})
	}
}
func TestScheduleMonitorLostResponseAfterCommitAndConcurrentRetry(t *testing.T) {
	f := newMonitorFixture(t)
	// The caller discards success, then restarts without the response.
	if _, state, err := f.deliver(f.lead); state != "notified" || err != nil {
		t.Fatal(state, err)
	}
	first := f.messages(t)[0]
	if first.PostReceipt == nil || !strings.HasPrefix(first.PostReceipt.RequestID, "schedule-monitor:v2:") {
		t.Fatalf("receipt %+v", first)
	}
	f.reopen(t)
	f.now = f.now.Add(time.Second)
	n, state, err := f.deliver(f.lead)
	if err != nil || state != "suppressed" || n.MessageSeq != first.Seq {
		t.Fatalf("lost-response %+v %s %v", n, state, err)
	}
	again := f.messages(t)
	if len(again) != 1 || !reflect.DeepEqual(first.PostReceipt, again[0].PostReceipt) {
		t.Fatal("receipt changed")
	}
	f.now = f.now.Add(time.Minute)
	start := make(chan struct{})
	states := make(chan string, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, state, err := f.deliver(f.lead); states <- state; errs <- err }()
	}
	close(start)
	wg.Wait()
	close(states)
	close(errs)
	notified := 0
	for state := range states {
		if state == "notified" {
			notified++
		} else if state != "suppressed" {
			t.Fatal(state)
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if notified != 1 || len(f.messages(t)) != 2 {
		t.Fatal("concurrent duplicate")
	}
}
func TestScheduleMonitorFailureRollbackRestartAndFrozenRetry(t *testing.T) {
	f := newMonitorFixture(t)
	f.sql(t, `CREATE TRIGGER monitor_fail BEFORE INSERT ON events WHEN NEW.kind='message' BEGIN SELECT RAISE(ABORT,'synthetic event failure'); END`)
	n, state, err := f.deliver(f.lead)
	if err == nil || state != "delivery_failed" || n.FailureCount != 1 || n.PendingRequestID == "" || len(f.messages(t)) != 0 {
		t.Fatalf("failure %+v %s %v", n, state, err)
	}
	pending := n.PendingRequestID
	f.reopen(t)
	f.now = f.now.Add(30 * time.Second)
	n, state, err = f.deliver(f.lead)
	if err != nil || state != "suppressed" || n.PendingRequestID != pending || n.FailureCount != 1 {
		t.Fatalf("restart %+v %s %v", n, state, err)
	}
	f.sql(t, `DROP TRIGGER monitor_fail`)
	f.now = f.now.Add(30 * time.Second)
	n, state, err = f.deliver(f.lead)
	if err != nil || state != "notified" || n.PendingRequestID != "" || n.FailureCount != 0 {
		t.Fatalf("recovery %+v %s %v", n, state, err)
	}
	m := f.messages(t)
	if len(m) != 1 || m[0].PostReceipt.RequestID != pending {
		t.Fatalf("retry changed %+v", m)
	}
}
func TestScheduleMonitorSuccessStateFailureRollsBackMailAndGeneration(t *testing.T) {
	f := newMonitorFixture(t)
	f.sql(t, `CREATE TRIGGER monitor_state_fail BEFORE INSERT ON schedule_monitor_notices BEGIN SELECT RAISE(ABORT,'synthetic state failure'); END`)
	if _, state, err := f.deliver(f.lead); err == nil || state != "unknown" {
		t.Fatal(state, err)
	}
	if len(f.messages(t)) != 0 {
		t.Fatal("orphan mail")
	}
	var next int64
	if err := f.s.db.QueryRow(`SELECT next_generation FROM schedule_monitor_generation`).Scan(&next); err != nil || next != 1 {
		t.Fatalf("generation %d %v", next, err)
	}
	f.reopen(t)
	f.sql(t, `DROP TRIGGER monitor_state_fail`)
	if _, state, err := f.deliver(f.lead); state != "notified" || err != nil {
		t.Fatal(state, err)
	}
	if len(f.messages(t)) != 1 {
		t.Fatal("not exactly once")
	}
}
func TestScheduleMonitorPruneReobserveAndRecipientChange(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivered", true: "failed"}[failed], func(t *testing.T) {
			f := newMonitorFixture(t)
			if failed {
				f.sql(t, `CREATE TRIGGER monitor_fail BEFORE INSERT ON events WHEN NEW.kind='message' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
			}
			n, state, err := f.deliver(f.lead)
			if failed {
				if state != "delivery_failed" || err == nil {
					t.Fatal("expected failure")
				}
			} else if state != "notified" || err != nil {
				t.Fatal(state, err)
			}
			oldID := n.PendingRequestID
			if !failed {
				oldID = f.messages(t)[0].PostReceipt.RequestID
			}
			f.now = f.now.Add(8 * 24 * time.Hour)
			count, err := f.s.PruneScheduleMonitorNotices(context.Background(), f.now.Add(-7*24*time.Hour))
			if err != nil || count != 1 {
				t.Fatalf("prune %d %v", count, err)
			}
			f.reopen(t)
			if failed {
				f.sql(t, `DROP TRIGGER monitor_fail`)
			}
			if _, state, err = f.deliver(f.lead); state != "notified" || err != nil {
				t.Fatal(state, err)
			}
			m := f.messages(t)
			last := m[len(m)-1]
			if last.PostReceipt.RequestID == oldID {
				t.Fatal("prune reused generation")
			}
			replacement, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: "replacement", Host: "fixture", Session: "replacement"}, monitorCaller)
			if err != nil {
				t.Fatal(err)
			}
			name := "replacement"
			if _, err = f.s.UpdateTask(context.Background(), f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, monitorCaller); err != nil {
				t.Fatal(err)
			}
			if _, state, err = f.deliver(replacement); state != "notified" || err != nil {
				t.Fatal(state, err)
			}
			m = f.messages(t)
			fresh := m[len(m)-1]
			if fresh.To != replacement.ID || fresh.PostReceipt.RequestID == last.PostReceipt.RequestID {
				t.Fatal("recipient reused identity")
			}
		})
	}
}
func TestScheduleMonitorOldSchemaMigration(t *testing.T) {
	f := newMonitorFixture(t)
	f.sql(t, `DROP TABLE schedule_monitor_notices`)
	f.sql(t, `DROP TABLE schedule_monitor_generation`)
	f.sql(t, `CREATE TABLE schedule_monitor_notices(task_id TEXT NOT NULL REFERENCES tasks(id),fingerprint TEXT NOT NULL,first_detected_at TEXT NOT NULL,last_observed_at TEXT NOT NULL,last_notified_at TEXT NOT NULL DEFAULT '',notification_count INTEGER NOT NULL DEFAULT 0,message_seq INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',PRIMARY KEY(task_id,fingerprint))`)
	legacy, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{To: f.lead.ID, Text: "GO! legacy", RequestID: "schedule-monitor:" + monitorFingerprint + ":1"}, api.Caller{Node: "system", User: "schedule-monitor"})
	if err != nil {
		t.Fatal(err)
	}
	f.sql(t, `INSERT INTO schedule_monitor_notices VALUES(?,?,?,?,?,1,?,'')`, f.task.ID, monitorFingerprint, ts(f.now), ts(f.now), ts(f.now), legacy.Seq)
	f.reopen(t)
	n, state, err := f.deliver(f.lead)
	if err != nil || state != "suppressed" || n.MessageSeq != legacy.Seq || !n.LastAttemptAt.Equal(f.now) {
		t.Fatalf("legacy %+v %s %v", n, state, err)
	}
	f.now = f.now.Add(time.Minute)
	if _, state, err = f.deliver(f.lead); err != nil || state != "notified" {
		t.Fatal(state, err)
	}
	m := f.messages(t)
	if len(m) != 2 || !strings.HasPrefix(m[1].PostReceipt.RequestID, "schedule-monitor:v2:") {
		t.Fatalf("legacy replay %+v", m)
	}
	f.reopen(t)
	if _, state, err = f.deliver(f.lead); err != nil || state != "suppressed" {
		t.Fatal(state, err)
	}
}
func TestScheduleMonitorInternalNoResumePolicy(t *testing.T) {
	f := newMonitorFixture(t)
	f.sql(t, `UPDATE agents SET last_seen_at=? WHERE id=?`, ts(time.Now().UTC()), f.lead.ID)
	f.status(t, api.AgentRetired)
	target, _ := f.s.GetAgent(context.Background(), f.lead.ID)
	tx, err := f.s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = f.s.insertMessageWithResume(context.Background(), tx, f.task, api.PostMessageRequest{To: target.ID, Text: "GO! internal"}, target, api.Caller{Node: "system", User: "schedule-monitor"}, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after, _ := f.s.GetAgent(context.Background(), target.ID)
	if after.Status != api.AgentRetired {
		t.Fatal("notification resumed")
	}
	var count int
	if err = f.s.db.QueryRow(`SELECT count(*) FROM events WHERE kind=?`, api.EventResumed).Scan(&count); err != nil || count != 0 {
		t.Fatalf("resumed %d %v", count, err)
	}
}

func TestScheduleMonitorReceiptFailureAndPendingRecipientChange(t *testing.T) {
	f := newMonitorFixture(t)
	// Fail after message AND event insertion: neither can escape receipt failure.
	f.sql(t, `CREATE TRIGGER monitor_receipt_fail BEFORE INSERT ON message_post_requests BEGIN SELECT RAISE(ABORT,'synthetic receipt failure'); END`)
	n, state, err := f.deliver(f.lead)
	if err == nil || state != "delivery_failed" || n.PendingRequestID == "" || len(f.messages(t)) != 0 {
		t.Fatalf("receipt failure %+v %s %v", n, state, err)
	}
	pending := n.PendingRequestID
	var count int
	if err = f.s.db.QueryRow(`SELECT count(*) FROM events WHERE kind='message'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("orphan events %d %v", count, err)
	}
	f.reopen(t)
	f.sql(t, `DROP TRIGGER monitor_receipt_fail`)
	replacement, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: "replacement", Host: "fixture", Session: "replacement"}, monitorCaller)
	if err != nil {
		t.Fatal(err)
	}
	name := "replacement"
	if _, err = f.s.UpdateTask(context.Background(), f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, monitorCaller); err != nil {
		t.Fatal(err)
	}
	n, state, err = f.deliver(replacement)
	if err != nil || state != "notified" || n.RecipientRunID != replacement.RunID {
		t.Fatalf("replacement %+v %s %v", n, state, err)
	}
	mail := f.messages(t)
	if len(mail) != 1 || mail[0].To != replacement.ID || mail[0].PostReceipt.RequestID == pending {
		t.Fatal("pending payload identity reused")
	}
	if mail[0].From.AgentID != "" || mail[0].From.Node != "system" || mail[0].From.User != "schedule-monitor" {
		t.Fatal("wrong attribution")
	}
}

func TestScheduleMonitorStaleSnapshotCannotResetNewRecipientState(t *testing.T) {
	f := newMonitorFixture(t)
	old := f.lead
	if _, state, err := f.deliver(old); err != nil || state != "notified" {
		t.Fatal(state, err)
	}
	replacement, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: "replacement", Host: "fixture", Session: "replacement"}, monitorCaller)
	if err != nil {
		t.Fatal(err)
	}
	name := "replacement"
	if _, err = f.s.UpdateTask(context.Background(), f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, monitorCaller); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	before, state, err := f.deliver(replacement)
	if err != nil || state != "notified" {
		t.Fatal(state, err)
	}
	f.now = f.now.Add(-time.Second) // stale observation time must not move retention backwards
	after, state, err := f.deliver(old)
	if err == nil || state != "unknown" {
		t.Fatal(state, err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stale snapshot reset state: %+v => %+v", before, after)
	}
	n, state, err := f.deliver(replacement)
	if err != nil || state != "suppressed" || n.MessageSeq != before.MessageSeq || len(f.messages(t)) != 2 {
		t.Fatalf("duplicate after stale observation: %+v %s %v", n, state, err)
	}
}
