// Package store persists tasks, agents, messages, and events in SQLite and
// notifies long-poll waiters when a task changes.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Global is the notifier key for changes to any task.
const Global = "*"

type Store struct {
	writeMu   sync.Mutex
	MaxAgents int
	db        *sql.DB
	mu        sync.Mutex
	waiters   map[string]chan struct{}
	now       func() time.Time
}

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, goal TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL, created_at TEXT NOT NULL, created_node TEXT NOT NULL,
  created_user TEXT NOT NULL, closed_at TEXT
);
CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id),
  name TEXT NOT NULL, host TEXT NOT NULL, session TEXT NOT NULL,
  runtime TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '',
  parent_agent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, last_event_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS agents_task ON agents(task_id);
CREATE TABLE IF NOT EXISTS messages (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id),
  from_agent TEXT NOT NULL DEFAULT '', from_node TEXT NOT NULL, from_user TEXT NOT NULL,
  to_agent TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_task ON messages(task_id, seq);
CREATE TABLE IF NOT EXISTS events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL,
  kind TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', text TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL DEFAULT '', by_node TEXT NOT NULL, by_user TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_task ON events(task_id, seq);
CREATE TABLE IF NOT EXISTS read_cursors (
  task_id TEXT NOT NULL, agent_id TEXT NOT NULL, up_to INTEGER NOT NULL,
  PRIMARY KEY (task_id, agent_id)
);
`

// Open opens or creates the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{MaxAgents: api.MaxAgentsPerTask, db: db, waiters: map[string]chan struct{}{}, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Changed returns a channel closed the next time the given task (or Global) changes.
func (s *Store) Changed(key string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.waiters[key]
	if !ok {
		ch = make(chan struct{})
		s.waiters[key] = ch
	}
	return ch
}

func (s *Store) notify(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range []string{taskID, Global} {
		if ch, ok := s.waiters[key]; ok {
			close(ch)
			delete(s.waiters, key)
		}
	}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Tasks

func (s *Store) CreateTask(ctx context.Context, req api.CreateTaskRequest, by api.Caller) (api.Task, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidTaskName(req.Name) || !api.ValidText(req.Goal, api.MaxTextLen) {
		return api.Task{}, api.ErrInvalid
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE status='open'`).Scan(&count); err != nil {
		return api.Task{}, err
	}
	if count >= api.MaxTasks {
		return api.Task{}, api.ErrLimit
	}
	maxNewAgents := 2
	if req.MaxNewAgents != nil {
		maxNewAgents = *req.MaxNewAgents
	}
	if maxNewAgents < 0 || maxNewAgents > api.MaxAgentsPerTask {
		return api.Task{}, api.ErrInvalid
	}
	if req.Orchestrator != "" && !api.ValidName(req.Orchestrator) {
		return api.Task{}, api.ErrInvalid
	}
	t := api.Task{Orchestrator: req.Orchestrator, Swarm: req.Swarm, MaxNewAgents: maxNewAgents, AllowAgentSpawn: req.AllowAgentSpawn, ID: api.NewID("tsk"), Name: req.Name, Goal: req.Goal, Status: api.TaskOpen, CreatedAt: s.now(), CreatedBy: by}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id,name,goal,status,created_at,created_node,created_user,allow_agent_spawn,max_new_agents,swarm,orchestrator) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Goal, t.Status, ts(t.CreatedAt), by.Node, by.User, t.AllowAgentSpawn, t.MaxNewAgents, t.Swarm, t.Orchestrator)
	if err != nil {
		return api.Task{}, err
	}
	if _, err := s.addEvent(ctx, t.ID, api.EventTaskCreated, "", t.Name, nil, by); err != nil {
		return api.Task{}, err
	}
	return t, nil
}

func scanTask(row interface{ Scan(...any) error }) (api.Task, error) {
	var t api.Task
	var created string
	var closed sql.NullString
	err := row.Scan(&t.ID, &t.Name, &t.Goal, &t.Status, &created, &t.CreatedBy.Node, &t.CreatedBy.User, &closed, &t.AllowAgentSpawn, &t.MaxNewAgents, &t.Swarm, &t.Orchestrator)
	if err != nil {
		return t, err
	}
	t.CreatedAt = parseTS(created)
	if closed.Valid {
		c := parseTS(closed.String)
		t.ClosedAt = &c
	}
	return t, nil
}

const taskCols = `id,name,goal,status,created_at,created_node,created_user,closed_at,allow_agent_spawn,max_new_agents,swarm,orchestrator`

func (s *Store) GetTask(ctx context.Context, id string) (api.Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, api.ErrNotFound
	}
	return t, err
}

func (s *Store) ListTasks(ctx context.Context) ([]api.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTask(ctx context.Context, id string, req api.UpdateTaskRequest, by api.Caller) (api.Task, error) {
	if req.Status != nil && *req.Status == api.TaskClosed {
		return s.CloseTask(ctx, id, by)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return t, err
	}
	if req.Name != nil {
		if !api.ValidTaskName(*req.Name) {
			return t, api.ErrInvalid
		}
		t.Name = *req.Name
	}
	if req.Goal != nil {
		if !api.ValidText(*req.Goal, api.MaxTextLen) {
			return t, api.ErrInvalid
		}
		t.Goal = *req.Goal
	}
	if req.Status != nil {
		switch *req.Status {
		case api.TaskOpen:
			t.Status = api.TaskOpen
			t.ClosedAt = nil
		default:
			return t, api.ErrInvalid
		}
	}
	if req.Orchestrator != nil {
		if *req.Orchestrator != "" && !api.ValidName(*req.Orchestrator) {
			return api.Task{}, api.ErrInvalid
		}
		t.Orchestrator = *req.Orchestrator
	}
	if req.Swarm != nil {
		t.Swarm = *req.Swarm
	}
	if req.AllowAgentSpawn != nil {
		t.AllowAgentSpawn = *req.AllowAgentSpawn
	}
	if req.MaxNewAgents != nil {
		if *req.MaxNewAgents < 0 || *req.MaxNewAgents > api.MaxAgentsPerTask {
			return t, api.ErrInvalid
		}
		t.MaxNewAgents = *req.MaxNewAgents
	}
	var closedAt any
	if t.ClosedAt != nil {
		closedAt = ts(*t.ClosedAt)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET name=?, goal=?, status=?, closed_at=?, allow_agent_spawn=?,max_new_agents=?,swarm=?,orchestrator=? WHERE id=?`, t.Name, t.Goal, t.Status, closedAt, t.AllowAgentSpawn, t.MaxNewAgents, t.Swarm, t.Orchestrator, id)
	if err == nil {
		_, err = s.addEvent(ctx, id, "task_updated", "", t.Name, nil, by)
	}
	return t, err
}

// CloseTask closes every open agent and then the task.
func (s *Store) CloseTask(ctx context.Context, id string, by api.Caller) (api.Task, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	t, err := s.GetTask(ctx, id)
	if err != nil {
		return t, err
	}
	agents, err := s.ListAgents(ctx, id)
	if err != nil {
		return t, err
	}
	for _, a := range agents {
		if a.Status != api.AgentClosed {
			if _, err := s.setAgentStatus(ctx, a.ID, api.AgentClosed, by, true); err != nil {
				return t, err
			}
		}
	}
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status=?, closed_at=? WHERE id=?`, api.TaskClosed, ts(now), id); err != nil {
		return t, err
	}
	t.Status = api.TaskClosed
	t.ClosedAt = &now
	_, err = s.addEvent(ctx, id, api.EventTaskClosed, "", t.Name, nil, by)
	return t, err
}

// Agents

const agentCols = `id,task_id,name,host,session,runtime,cwd,parent_agent_id,status,title,created_at,last_event_at,run_id,last_seen_at,blocked_reason,blocked_text`

func scanAgent(row interface{ Scan(...any) error }) (api.Agent, error) {
	var a api.Agent
	var created, last, seen string
	err := row.Scan(&a.ID, &a.TaskID, &a.Name, &a.Host, &a.Session, &a.Runtime, &a.Cwd, &a.ParentAgentID, &a.Status, &a.Title, &created, &last, &a.RunID, &seen, &a.BlockedReason, &a.BlockedText)
	a.CreatedAt, a.LastEventAt = parseTS(created), parseTS(last)
	a.LastSeenAt = parseTS(seen)
	a.Online = !a.LastSeenAt.IsZero() && time.Since(a.LastSeenAt) < 90*time.Second && a.Status != api.AgentExited && a.Status != api.AgentClosed
	return a, err
}

func (s *Store) AddAgent(ctx context.Context, taskID string, req api.AddAgentRequest, by api.Caller) (api.Agent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return api.Agent{}, err
	}
	if t.Status != api.TaskOpen {
		return api.Agent{}, api.ErrClosed
	}
	if req.ParentAgentID != "" {
		parent, err := s.GetAgent(ctx, req.ParentAgentID)
		if err != nil || parent.TaskID != taskID || parent.Status == api.AgentClosed || parent.Status == api.AgentExited || parent.Status == api.AgentRetired {
			return api.Agent{}, api.ErrInvalid
		}
		if !t.AllowAgentSpawn {
			return api.Agent{}, api.ErrAgentSpawnDisabled
		}
	}
	if req.AgentID != "" {
		if !api.ValidID(req.AgentID, "agt") {
			return api.Agent{}, api.ErrInvalid
		}
		if existing, err := s.GetAgent(ctx, req.AgentID); err == nil {
			if existing.TaskID != taskID {
				return api.Agent{}, api.ErrInvalid
			}
			return existing, nil
		}
	}
	if !api.ValidName(req.Name) || !api.ValidName(req.Session) || req.Host == "" ||
		!api.ValidText(req.Host, 253) || !api.ValidText(req.Runtime, 128) || !api.ValidText(req.Cwd, 1024) {
		return api.Agent{}, api.ErrInvalid
	}
	if req.ParentAgentID != "" && !api.ValidID(req.ParentAgentID, "agt") {
		return api.Agent{}, api.ErrInvalid
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE task_id=? AND status NOT IN ('closed','exited')`, taskID).Scan(&count); err != nil {
		return api.Agent{}, err
	}
	if count >= s.MaxAgents {
		return api.Agent{}, api.ErrLimit
	}
	// An exited participant may start a new run, retaining identity and inbox.
	var previousID string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM agents WHERE task_id=? AND name=? AND status<>'closed' ORDER BY created_at LIMIT 1`, taskID, req.Name).Scan(&previousID)
	if err == nil {
		previous, e := s.GetAgent(ctx, previousID)
		if e != nil {
			return api.Agent{}, e
		}
		if previous.Status != api.AgentExited || previous.Host != req.Host || previous.ParentAgentID != req.ParentAgentID {
			return api.Agent{}, api.ErrLimit
		}
		runID := api.NewID("run")
		_, err = s.db.ExecContext(ctx, `UPDATE agents SET session=?,runtime=?,cwd=?,run_id=?,status='starting',last_seen_at='' WHERE id=?`, req.Session, req.Runtime, req.Cwd, runID, previousID)
		if err != nil {
			return api.Agent{}, err
		}
		_, err = s.addEvent(ctx, taskID, api.EventAgentAdded, previousID, req.Name, map[string]any{"runId": runID}, by)
		if err != nil {
			return api.Agent{}, err
		}
		return s.GetAgent(ctx, previousID)
	}
	if err != sql.ErrNoRows {
		return api.Agent{}, err
	}
	if req.ParentAgentID != "" {
		var helpers int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE task_id=? AND parent_agent_id<>''`, taskID).Scan(&helpers); err != nil {
			return api.Agent{}, err
		}
		if helpers >= t.MaxNewAgents {
			return api.Agent{}, api.ErrAgentSpawnLimit
		}
	}
	now := s.now()
	a := api.Agent{RunID: api.NewID("run"), ID: req.AgentID, TaskID: taskID, Name: req.Name, Host: req.Host, Session: req.Session, Runtime: req.Runtime,
		Cwd: req.Cwd, ParentAgentID: req.ParentAgentID, Status: api.AgentStarting, CreatedAt: now, LastEventAt: now}
	if a.ID == "" {
		a.ID = api.NewID("agt")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.TaskID, a.Name, a.Host, a.Session, a.Runtime, a.Cwd, a.ParentAgentID, a.Status, a.Title, ts(now), ts(now), a.RunID, "", "", "")
	if err != nil {
		return a, err
	}
	_, err = s.addEvent(ctx, taskID, api.EventAgentAdded, a.ID, a.Name, map[string]any{"host": a.Host, "session": a.Session, "runtime": a.Runtime, "parentAgentId": a.ParentAgentID}, by)
	return a, err
}

func (s *Store) GetAgent(ctx context.Context, id string) (api.Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, api.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if a.ReadUpTo, err = s.ReadCursor(ctx, a.TaskID, a.ID); err != nil {
		return a, err
	}
	a.Unread, err = s.Unread(ctx, a.TaskID, a.ID)
	return a, err
}

func (s *Store) ListAgents(ctx context.Context, taskID string) ([]api.Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].ReadUpTo, err = s.ReadCursor(ctx, taskID, out[i].ID); err != nil {
			return nil, err
		}
		if out[i].Unread, err = s.Unread(ctx, taskID, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) UpdateAgent(ctx context.Context, id string, req api.UpdateAgentRequest, by api.Caller) (api.Agent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	a, err := s.GetAgent(ctx, id)
	if err != nil {
		return a, err
	}
	if req.Title != nil {
		if !api.ValidText(*req.Title, 200) {
			return a, api.ErrInvalid
		}
		a.Title = *req.Title
		if _, err := s.db.ExecContext(ctx, `UPDATE agents SET title=? WHERE id=?`, a.Title, id); err != nil {
			return a, err
		}
		s.notify(a.TaskID)
	}
	if req.Status != nil {
		if (a.Status == api.AgentClosed || a.Status == api.AgentExited) && (*req.Status == api.AgentRetired || *req.Status == api.AgentDone) {
			return a, api.ErrClosed
		}
		task, err := s.GetTask(ctx, a.TaskID)
		if err != nil {
			return a, err
		}
		if task.Status != api.TaskOpen {
			return a, api.ErrClosed
		}
		switch *req.Status {
		case api.AgentRunning, api.AgentDone, api.AgentNeedsInput, api.AgentClosed, api.AgentExited, api.AgentRetired:
		default:
			return a, api.ErrInvalid
		}
		return s.setAgentStatus(ctx, id, *req.Status, by, true)
	}
	return a, nil
}

// CloseAgent marks an agent closed and emits a closed event.
func (s *Store) CloseAgent(ctx context.Context, id string, by api.Caller) (api.Agent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.setAgentStatus(ctx, id, api.AgentClosed, by, true)
}

func (s *Store) setAgentStatus(ctx context.Context, id, status string, by api.Caller, emit bool) (api.Agent, error) {
	a, err := s.GetAgent(ctx, id)
	if err != nil {
		return a, err
	}
	previousStatus := a.Status
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `UPDATE agents SET status=?, last_event_at=?,blocked_reason='',blocked_text='' WHERE id=?`, status, ts(now), id); err != nil {
		return a, err
	}
	a.Status, a.LastEventAt = status, now
	a.BlockedReason, a.BlockedText = "", ""
	if emit {
		kind := map[string]string{api.AgentRunning: api.EventRunning, api.AgentDone: api.EventDone, api.AgentRetired: api.EventRetired, api.AgentNeedsInput: api.EventNeedsInput, api.AgentClosed: api.EventClosed, api.AgentExited: api.EventExited}[status]
		if previousStatus == api.AgentRetired && status == api.AgentDone {
			kind = api.EventResumed
		}
		if _, err := s.addEvent(ctx, a.TaskID, kind, id, "", nil, by); err != nil {
			return a, err
		}
	} else {
		s.notify(a.TaskID)
	}
	return a, nil
}

// Messages

func (s *Store) PostMessage(ctx context.Context, taskID string, req api.PostMessageRequest, by api.Caller) (api.Message, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return api.Message{}, err
	}
	if t.Status != api.TaskOpen {
		return api.Message{}, api.ErrClosed
	}
	if req.Text == "" || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.Message{}, api.ErrInvalid
	}
	for _, id := range []string{req.To, req.AgentID} {
		if id == "" {
			continue
		}
		a, err := s.GetAgent(ctx, id)
		if err != nil || a.TaskID != taskID {
			return api.Message{}, api.ErrInvalid
		}
	}
	if req.ReplyTo < 0 {
		return api.Message{}, api.ErrInvalid
	}
	if req.ReplyTo > 0 {
		var replyTask string
		if err := s.db.QueryRowContext(ctx, `SELECT task_id FROM messages WHERE seq=?`, req.ReplyTo).Scan(&replyTask); err != nil || replyTask != taskID {
			return api.Message{}, api.ErrInvalid
		}
	}
	m := api.Message{Broadcast: t.Swarm, ReplyTo: req.ReplyTo, TaskID: taskID, From: api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}, To: req.To, Text: req.Text, CreatedAt: s.now()}
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages (task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast) VALUES (?,?,?,?,?,?,?,?,?)`,
		taskID, req.AgentID, by.Node, by.User, req.To, req.Text, ts(m.CreatedAt), req.ReplyTo, m.Broadcast)
	if err != nil {
		return m, err
	}
	m.Seq, _ = res.LastInsertId()
	preview := req.Text
	if len(preview) > 200 {
		preview = preview[:200]
	}
	_, err = s.addEvent(ctx, taskID, api.EventMessage, req.AgentID, preview, map[string]any{"seq": m.Seq, "to": req.To, "broadcast": m.Broadcast}, by)
	return m, err
}

// ListMessages returns messages after seq. When agentID is set, only broadcast
// messages and messages addressed to that agent are returned.
func (s *Store) ListMessages(ctx context.Context, taskID string, after int64, agentID string, limit int) ([]api.Message, error) {
	if limit <= 0 || limit > api.MaxLimit {
		limit = api.MaxLimit
	}
	q := `SELECT seq,task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast FROM messages WHERE task_id=? AND seq>?`
	args := []any{taskID, after}
	if agentID != "" {
		q += ` AND (broadcast=1 OR to_agent='' OR to_agent=?)`
		args = append(args, agentID)
	}
	if after < 0 {
		q += ` ORDER BY seq DESC LIMIT ?`
	} else {
		q += ` ORDER BY seq LIMIT ?`
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Message{}
	for rows.Next() {
		var m api.Message
		var created string
		if err := rows.Scan(&m.Seq, &m.TaskID, &m.From.AgentID, &m.From.Node, &m.From.User, &m.To, &m.Text, &created, &m.ReplyTo, &m.Broadcast); err != nil {
			return nil, err
		}
		m.CreatedAt = parseTS(created)
		out = append(out, m)
	}
	if after < 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, rows.Err()
}

func (s *Store) ReadCursor(ctx context.Context, taskID, agentID string) (int64, error) {
	var upTo int64
	err := s.db.QueryRowContext(ctx, `SELECT up_to FROM read_cursors WHERE task_id=? AND agent_id=?`, taskID, agentID).Scan(&upTo)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return upTo, err
}

func (s *Store) MarkRead(ctx context.Context, taskID string, req api.MarkReadRequest) error {
	if !api.ValidID(req.AgentID, "agt") || req.UpTo < 0 {
		return api.ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO read_cursors (task_id,agent_id,up_to) VALUES (?,?,?)
		ON CONFLICT(task_id,agent_id) DO UPDATE SET up_to=MAX(up_to, excluded.up_to)`, taskID, req.AgentID, req.UpTo)
	if err == nil {
		s.notify(taskID)
	}
	return err
}

// Unread counts messages for agentID that it has not read and did not send.
func (s *Store) Unread(ctx context.Context, taskID, agentID string) (int, error) {
	cursor, err := s.ReadCursor(ctx, taskID, agentID)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE task_id=? AND seq>? AND from_agent<>? AND (broadcast=1 OR to_agent='' OR to_agent=?)`,
		taskID, cursor, agentID, agentID).Scan(&n)
	return n, err
}

// Events

func (s *Store) PostEvent(ctx context.Context, taskID string, req api.PostEventRequest, by api.Caller) (api.Event, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.PostableKind(req.Kind) || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.Event{}, api.ErrInvalid
	}
	if req.Data != nil {
		if b, err := json.Marshal(req.Data); err != nil || len(b) > api.MaxDataLen {
			return api.Event{}, api.ErrInvalid
		}
	}
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return api.Event{}, err
	}
	if t.Status != api.TaskOpen {
		return api.Event{}, api.ErrClosed
	}
	if req.AgentID != "" {
		a, err := s.GetAgent(ctx, req.AgentID)
		if err != nil || a.TaskID != taskID {
			return api.Event{}, api.ErrInvalid
		}
		if req.RunID != "" && req.RunID != a.RunID {
			return api.Event{}, api.ErrInvalid
		}
		if a.Status == api.AgentExited || a.Status == api.AgentClosed {
			return api.Event{}, api.ErrClosed
		}
		if req.RunID != "" {
			if _, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at=? WHERE id=?`, ts(s.now()), a.ID); err != nil {
				return api.Event{}, err
			}
		}
		if status := api.LifecycleStatus(req.Kind); status != "" && a.Status != status && !(a.Status == api.AgentRetired && status != api.AgentClosed && status != api.AgentExited) && !(req.Kind == api.EventDone && req.Data["runtimeStop"] == true && a.Status == api.AgentNeedsInput) {
			if _, err := s.setAgentStatus(ctx, req.AgentID, status, by, false); err != nil {
				return api.Event{}, err
			}
		}
		if req.Kind == api.EventNeedsInput && a.Status != api.AgentRetired {
			reason, _ := req.Data["reason"].(string)
			if reason != "permission" && reason != "authentication" && reason != "tool" {
				reason = ""
			}
			if reason == "" {
				switch {
				case strings.HasPrefix(req.Text, "Permission blocked"):
					reason = "permission"
				case strings.HasPrefix(req.Text, "Login required"):
					reason = "authentication"
				case strings.HasPrefix(req.Text, "Tool unavailable"):
					reason = "tool"
				}
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE agents SET blocked_reason=?,blocked_text=? WHERE id=?`, reason, req.Text, a.ID); err != nil {
				return api.Event{}, err
			}
		}

	}
	return s.addEvent(ctx, taskID, req.Kind, req.AgentID, req.Text, req.Data, by)
}

func (s *Store) addEvent(ctx context.Context, taskID, kind, agentID, text string, data map[string]any, by api.Caller) (api.Event, error) {
	e := api.Event{TaskID: taskID, Kind: kind, AgentID: agentID, Text: text, Data: data, By: by, CreatedAt: s.now()}
	encoded := ""
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return e, err
		}
		encoded = string(b)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events (task_id,kind,agent_id,text,data,by_node,by_user,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		taskID, kind, agentID, text, encoded, by.Node, by.User, ts(e.CreatedAt))
	if err != nil {
		return e, err
	}
	e.Seq, _ = res.LastInsertId()
	if agentID != "" {
		_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_event_at=? WHERE id=?`, ts(e.CreatedAt), agentID)
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM events WHERE task_id=? AND seq <= (SELECT seq FROM events WHERE task_id=? ORDER BY seq DESC LIMIT 1 OFFSET ?)`,
		taskID, taskID, api.MaxEventsPerTask)
	s.notify(taskID)
	return e, nil
}

// ListEvents returns events after seq for one task, or for all tasks when taskID is "".
func (s *Store) ListEvents(ctx context.Context, taskID string, after int64, limit int) ([]api.Event, error) {
	if limit <= 0 || limit > api.MaxLimit {
		limit = api.MaxLimit
	}
	q := `SELECT seq,task_id,kind,agent_id,text,data,by_node,by_user,created_at FROM events WHERE seq>?`
	args := []any{after}
	if taskID != "" {
		q += ` AND task_id=?`
		args = append(args, taskID)
	}
	q += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Event{}
	for rows.Next() {
		var e api.Event
		var data, created string
		if err := rows.Scan(&e.Seq, &e.TaskID, &e.Kind, &e.AgentID, &e.Text, &data, &e.By.Node, &e.By.User, &created); err != nil {
			return nil, err
		}
		if data != "" {
			_ = json.Unmarshal([]byte(data), &e.Data)
		}
		e.CreatedAt = parseTS(created)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestSeq returns the newest event sequence number for a task (or globally).
func (s *Store) LatestSeq(ctx context.Context, taskID string) (int64, error) {
	var seq sql.NullInt64
	var err error
	if taskID == "" {
		err = s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&seq)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE task_id=?`, taskID).Scan(&seq)
	}
	return seq.Int64, err
}

// WaitEvents long-polls: it returns immediately when events exist after seq and
// otherwise waits for a change, the timeout, or context cancellation.
func (s *Store) WaitEvents(ctx context.Context, taskID string, after int64, limit int, wait time.Duration) ([]api.Event, error) {
	key := taskID
	if key == "" {
		key = Global
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		changed := s.Changed(key)
		events, err := s.ListEvents(ctx, taskID, after, limit)
		if err != nil || len(events) > 0 {
			return events, err
		}
		select {
		case <-changed:
		case <-deadline.C:
			return events, nil
		case <-ctx.Done():
			return events, nil
		}
	}
}
