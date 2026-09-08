package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type resumeFixture struct {
	s      *Store
	path   string
	ctx    context.Context
	by     api.Caller
	task   api.Task
	parent api.Agent
	target api.Agent
}

func newResumeFixture(t *testing.T, swarm, heartbeat bool) resumeFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	by := api.Caller{Node: "test-host", User: "owner"}
	maxNewAgents := 1
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Resume", AllowAgentSpawn: true, MaxNewAgents: &maxNewAgents, Swarm: swarm}, by)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "host", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "host", Session: "worker", Runtime: "codex", Cwd: "/project", ParentAgentID: parent.ID}, by)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat {
		if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: target.ID, RunID: target.RunID, Kind: api.EventHeartbeat}, by); err != nil {
			t.Fatal(err)
		}
	}
	retired := api.AgentRetired
	if _, err = s.UpdateAgent(ctx, target.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	return resumeFixture{s: s, path: path, ctx: ctx, by: by, task: task, parent: parent, target: target}
}

func TestHumanDirectMessageResumesSameOnlineRetiredRun(t *testing.T) {
	f := newResumeFixture(t, true, true)
	initial, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.parent.ID, Text: "Earlier broadcast"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.MarkRead(f.ctx, f.task.ID, api.MarkReadRequest{AgentID: f.target.ID, UpTo: initial.Seq}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.ExecContext(f.ctx, `UPDATE agents SET cleanup_done=1,cleanup_error='receipt retained' WHERE id=?`, f.target.ID); err != nil {
		t.Fatal(err)
	}
	before, err := f.s.GetAgent(f.ctx, f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != api.AgentRetired || !before.Online {
		t.Fatalf("fixture is not an online retired agent: %+v", before)
	}
	first, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: f.target.ID, Text: "Please continue"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if first.From.AgentID != "" || first.To != f.target.ID || !first.Broadcast {
		t.Fatalf("message contract changed: %+v", first)
	}
	// A retry is another accepted message, but it must not emit another resume.
	if _, err = f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: f.target.ID, Text: "Please continue"}, f.by); err != nil {
		t.Fatal(err)
	}
	after, err := f.s.GetAgent(f.ctx, f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != api.AgentDone || after.ID != before.ID || after.RunID != before.RunID || after.Session != before.Session || after.Runtime != before.Runtime || after.Cwd != before.Cwd || after.ParentAgentID != before.ParentAgentID {
		t.Fatalf("resume replaced or mutated the run: before=%+v after=%+v", before, after)
	}
	if after.ReadUpTo != initial.Seq || after.Unread != 2 || !after.CleanupDone || after.CleanupError != "receipt retained" {
		t.Fatalf("resume changed durable receipts/cursor: %+v", after)
	}
	agents, err := f.s.ListAgents(f.ctx, f.task.ID)
	if err != nil || len(agents) != 2 {
		t.Fatalf("resume recreated an identity: %d %v", len(agents), err)
	}
	if _, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "extra", Host: "host", Session: "extra", ParentAgentID: f.parent.ID}, f.by); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("resume reset helper allowance: %v", err)
	}
	events, err := f.s.ListEvents(f.ctx, f.task.ID, 0, api.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	resumeCount := 0
	for _, event := range events {
		if event.Kind == api.EventResumed && event.AgentID == f.target.ID {
			resumeCount++
		}
	}
	if resumeCount != 1 {
		t.Fatalf("resume events = %d, want 1", resumeCount)
	}

	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetAgent(f.ctx, f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != api.AgentDone || persisted.RunID != before.RunID || persisted.ReadUpTo != initial.Seq || !persisted.CleanupDone || persisted.CleanupError != "receipt retained" {
		t.Fatalf("resume did not persist coherently: %+v", persisted)
	}
}

func TestHumanDirectMessageResumeNegativeMatrix(t *testing.T) {
	tests := []struct {
		name       string
		swarm      bool
		heartbeat  bool
		prepare    func(*testing.T, resumeFixture) api.PostMessageRequest
		wantErr    error
		wantStatus string
	}{
		{name: "human broadcast", heartbeat: true, prepare: func(_ *testing.T, f resumeFixture) api.PostMessageRequest {
			return api.PostMessageRequest{Text: "Announcement"}
		}, wantStatus: api.AgentRetired},
		{name: "agent direct", heartbeat: true, prepare: func(_ *testing.T, f resumeFixture) api.PostMessageRequest {
			return api.PostMessageRequest{AgentID: f.parent.ID, To: f.target.ID, Text: "Peer request"}
		}, wantStatus: api.AgentRetired},
		{name: "agent swarm direct", swarm: true, heartbeat: true, prepare: func(_ *testing.T, f resumeFixture) api.PostMessageRequest {
			return api.PostMessageRequest{AgentID: f.parent.ID, To: f.target.ID, Text: "Peer request"}
		}, wantStatus: api.AgentRetired},
		{name: "offline retired", prepare: func(_ *testing.T, f resumeFixture) api.PostMessageRequest {
			return api.PostMessageRequest{To: f.target.ID, Text: "Human request"}
		}, wantStatus: api.AgentRetired},
		{name: "stale retired", heartbeat: true, prepare: func(t *testing.T, f resumeFixture) api.PostMessageRequest {
			if _, err := f.s.db.ExecContext(f.ctx, `UPDATE agents SET last_seen_at=? WHERE id=?`, ts(time.Now().Add(-2*time.Minute)), f.target.ID); err != nil {
				t.Fatal(err)
			}
			return api.PostMessageRequest{To: f.target.ID, Text: "Human request"}
		}, wantStatus: api.AgentRetired},
		{name: "closed agent", heartbeat: true, prepare: func(t *testing.T, f resumeFixture) api.PostMessageRequest {
			if _, err := f.s.CloseAgent(f.ctx, f.target.ID, f.by); err != nil {
				t.Fatal(err)
			}
			return api.PostMessageRequest{To: f.target.ID, Text: "Human request"}
		}, wantStatus: api.AgentClosed},
		{name: "exited agent", heartbeat: true, prepare: func(t *testing.T, f resumeFixture) api.PostMessageRequest {
			exited := api.AgentExited
			if _, err := f.s.UpdateAgent(f.ctx, f.target.ID, api.UpdateAgentRequest{Status: &exited}, f.by); err != nil {
				t.Fatal(err)
			}
			return api.PostMessageRequest{To: f.target.ID, Text: "Human request"}
		}, wantStatus: api.AgentExited},
		{name: "invalid message", heartbeat: true, prepare: func(_ *testing.T, f resumeFixture) api.PostMessageRequest {
			return api.PostMessageRequest{To: f.target.ID, Text: "bad\x00message"}
		}, wantErr: api.ErrInvalid, wantStatus: api.AgentRetired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newResumeFixture(t, test.swarm, test.heartbeat)
			_, err := f.s.PostMessage(f.ctx, f.task.ID, test.prepare(t, f), f.by)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("PostMessage error = %v, want %v", err, test.wantErr)
			}
			got, err := f.s.GetAgent(f.ctx, f.target.ID)
			if err != nil || got.Status != test.wantStatus {
				t.Fatalf("target after message: %+v, %v", got, err)
			}
		})
	}
}

func TestHumanDirectMessageResumeRejectsCrossTaskAndClosedTask(t *testing.T) {
	f := newResumeFixture(t, false, true)
	other, err := f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Other"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostMessage(f.ctx, other.ID, api.PostMessageRequest{To: f.target.ID, Text: "Wrong task"}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("cross-task message error = %v", err)
	}
	got, err := f.s.GetAgent(f.ctx, f.target.ID)
	if err != nil || got.Status != api.AgentRetired {
		t.Fatalf("cross-task request resumed target: %+v %v", got, err)
	}
	if _, err = f.s.CloseTask(f.ctx, f.task.ID, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: f.target.ID, Text: "Closed task"}, f.by); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("closed-task message error = %v", err)
	}
	got, err = f.s.GetAgent(f.ctx, f.target.ID)
	if err != nil || got.Status != api.AgentClosed {
		t.Fatalf("closed-task request changed target: %+v %v", got, err)
	}
}

func TestHumanDirectMessageAndResumeRollbackTogether(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "message insert", trigger: `CREATE TRIGGER fail_message BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'fail message'); END`},
		{name: "message event insert", trigger: `CREATE TRIGGER fail_message_event BEFORE INSERT ON events WHEN NEW.kind='message' BEGIN SELECT RAISE(ABORT, 'fail message event'); END`},
		{name: "resume event insert", trigger: `CREATE TRIGGER fail_resume_event BEFORE INSERT ON events WHEN NEW.kind='resumed' BEGIN SELECT RAISE(ABORT, 'fail resume'); END`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newResumeFixture(t, false, true)
			beforeEvents, err := f.s.ListEvents(f.ctx, f.task.ID, 0, api.MaxLimit)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.db.ExecContext(f.ctx, test.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: f.target.ID, Text: "Must be atomic"}, f.by); err == nil {
				t.Fatal("message unexpectedly succeeded")
			}
			got, err := f.s.GetAgent(f.ctx, f.target.ID)
			if err != nil || got.Status != api.AgentRetired {
				t.Fatalf("failed message changed target: %+v %v", got, err)
			}
			messages, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", api.MaxLimit)
			if err != nil || len(messages) != 0 {
				t.Fatalf("failed resume retained message: %+v %v", messages, err)
			}
			afterEvents, err := f.s.ListEvents(f.ctx, f.task.ID, 0, api.MaxLimit)
			if err != nil || len(afterEvents) != len(beforeEvents) {
				t.Fatalf("failed resume retained event: before=%d after=%d err=%v", len(beforeEvents), len(afterEvents), err)
			}
		})
	}
}
