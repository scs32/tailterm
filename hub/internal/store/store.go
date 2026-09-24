// Package store persists tasks, agents, messages, and events in SQLite and
// notifies long-poll waiters when a task changes.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Global is the notifier key for changes to any task.
const Global = "*"

type Store struct {
	writeMu    sync.Mutex
	jevEnabled atomic.Bool
	MaxAgents  int
	db         *sql.DB
	mu         sync.Mutex
	waiters    map[string]chan struct{}
	now        func() time.Time
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

func validRunID(id string) bool {
	if len(id) != 20 || !strings.HasPrefix(id, "run_") {
		return false
	}
	for _, r := range id[4:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
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
	t := api.Task{PauseState: api.ProjectPauseActive, Orchestrator: req.Orchestrator, Swarm: req.Swarm, MaxNewAgents: maxNewAgents, AllowAgentSpawn: req.AllowAgentSpawn, ID: api.NewID("tsk"), Name: req.Name, Goal: req.Goal, Status: api.TaskOpen, CreatedAt: s.now(), CreatedBy: by}
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
	var created, paused string
	var closed sql.NullString
	err := row.Scan(&t.ID, &t.Name, &t.Goal, &t.Status, &created, &t.CreatedBy.Node, &t.CreatedBy.User, &closed, &t.AllowAgentSpawn, &t.MaxNewAgents, &t.Swarm, &t.Orchestrator, &t.LeadRevision, &t.CleanupPending, &t.PauseState, &t.LifecycleGeneration, &t.PauseGeneration, &t.PauseCleanupPending, &t.PauseHandoffPending, &paused)
	if err != nil {
		return t, err
	}
	t.CreatedAt = parseTS(created)
	if closed.Valid {
		c := parseTS(closed.String)
		t.ClosedAt = &c
	}
	if paused != "" {
		p := parseTS(paused)
		t.PausedAt = &p
	}
	return t, nil
}

const taskCols = `tasks.id,tasks.name,tasks.goal,tasks.status,tasks.created_at,tasks.created_node,tasks.created_user,tasks.closed_at,tasks.allow_agent_spawn,tasks.max_new_agents,tasks.swarm,tasks.orchestrator,tasks.lead_revision,
CASE WHEN tasks.status='closed' THEN (SELECT count(*) FROM agents WHERE task_id=tasks.id AND cleanup_done=0) ELSE 0 END,
tasks.pause_state,tasks.lifecycle_generation,tasks.pause_generation,
CASE WHEN tasks.pause_state<>'active' THEN (SELECT count(*) FROM project_pause_targets pt JOIN project_pause_cycles pc ON pc.id=pt.cycle_id JOIN agents pa ON pa.id=pt.agent_id AND pa.run_id=pt.run_id WHERE pc.task_id=tasks.id AND pc.pause_generation=tasks.pause_generation AND pa.cleanup_done=0) ELSE 0 END,
CASE WHEN tasks.pause_state<>'active' THEN (SELECT count(*) FROM project_pause_targets pt JOIN project_pause_cycles pc ON pc.id=pt.cycle_id WHERE pc.task_id=tasks.id AND pc.pause_generation=tasks.pause_generation AND (pt.service_verified=0 OR pt.service_disposition='unresolved')) ELSE 0 END,
tasks.paused_at`

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
		if t.PauseState != api.ProjectPauseActive {
			return t, fmt.Errorf("%w: project lead changes are blocked while the project is paused", api.ErrConflict)
		}
		if *req.Orchestrator != "" && !api.ValidName(*req.Orchestrator) {
			return api.Task{}, api.ErrInvalid
		}
		if t.Orchestrator != *req.Orchestrator {
			t.LeadRevision++
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
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET name=?, goal=?, status=?, closed_at=?, allow_agent_spawn=?,max_new_agents=?,swarm=?,orchestrator=?,lead_revision=? WHERE id=?`, t.Name, t.Goal, t.Status, closedAt, t.AllowAgentSpawn, t.MaxNewAgents, t.Swarm, t.Orchestrator, t.LeadRevision, id)
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
	if t.Status == api.TaskClosed {
		return t, nil
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
	for _, a := range agents {
		if !a.CleanupDone {
			t.CleanupPending++
		}
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET status=?, closed_at=? WHERE id=?`, api.TaskClosed, ts(now), id); err != nil {
		return t, err
	}
	t.Status = api.TaskClosed
	t.ClosedAt = &now
	affectedQueues, err := s.cancelQueuesForProjectTx(ctx, tx, id, by)
	if err != nil {
		return t, err
	}
	if _, err = s.insertEvent(ctx, tx, id, api.EventTaskClosed, "", t.Name, nil, by); err != nil {
		return t, err
	}
	if err = tx.Commit(); err == nil {
		s.notify(id)
		for _, targetID := range affectedQueues {
			if targetID != id {
				s.notify(targetID)
			}
		}
	}
	return t, err
}

// Agents

const agentCols = `id,task_id,name,host,session,runtime,cwd,parent_agent_id,role,status,title,created_at,last_event_at,run_id,last_seen_at,blocked_reason,blocked_text,cleanup_done,cleanup_error`

func scanAgent(row interface{ Scan(...any) error }) (api.Agent, error) {
	var a api.Agent
	var created, last, seen string
	err := row.Scan(&a.ID, &a.TaskID, &a.Name, &a.Host, &a.Session, &a.Runtime, &a.Cwd, &a.ParentAgentID, &a.Role, &a.Status, &a.Title, &created, &last, &a.RunID, &seen, &a.BlockedReason, &a.BlockedText, &a.CleanupDone, &a.CleanupError)
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
	if t.PauseState == api.ProjectPauseActive && req.ResumeReceiptID != "" {
		var plan pendingResumeAdmission
		var admittedAt, state string
		err = s.db.QueryRowContext(ctx, `SELECT id,resume_receipt_id,resume_agent_id,resume_run_id,resume_name,selected_team_id,resume_admitted_at,state FROM project_pause_cycles WHERE task_id=? AND pause_generation=? AND resume_receipt_id=?`, taskID, t.PauseGeneration, req.ResumeReceiptID).Scan(
			&plan.CycleID, &plan.ReceiptID, &plan.AgentID, &plan.RunID, &plan.Name, &plan.SelectedTeam, &admittedAt, &state)
		if err != nil || state != "resumed" || admittedAt == "" || req.AgentID != plan.AgentID || req.ExpectedRunID != plan.RunID || req.Name != plan.Name || req.ParentAgentID != "" || req.Role != "" || req.WorkItem != nil || req.ExpectedLifecycleGeneration != t.LifecycleGeneration {
			return api.Agent{}, fmt.Errorf("%w: resume admission replay does not match the consumed exact lead run", api.ErrConflict)
		}
		existing, getErr := s.GetAgent(ctx, req.AgentID)
		if getErr != nil || existing.TaskID != taskID || existing.RunID != req.ExpectedRunID || existing.Name != req.Name {
			return api.Agent{}, fmt.Errorf("%w: resumed orchestrator run is not the saved exact admission", api.ErrConflict)
		}
		return existing, nil
	}
	var resumeAdmission *pendingResumeAdmission
	if t.PauseState == api.ProjectPauseResuming {
		plan, planErr := loadPendingResumeAdmission(ctx, s.db, taskID, t.PauseGeneration)
		if planErr != nil {
			return api.Agent{}, planErr
		}
		if req.ResumeReceiptID == "" || req.ResumeReceiptID != plan.ReceiptID || req.AgentID != plan.AgentID || req.ExpectedRunID != plan.RunID || req.Name != plan.Name || req.ParentAgentID != "" || req.Role != "" || req.WorkItem != nil {
			return api.Agent{}, fmt.Errorf("%w: only the exact planned fresh orchestrator may enter a resuming project", api.ErrConflict)
		}
		resumeAdmission = &plan
	} else if t.PauseState != api.ProjectPauseActive {
		return api.Agent{}, fmt.Errorf("%w: project is paused", api.ErrConflict)
	}
	if t.LifecycleGeneration != req.ExpectedLifecycleGeneration {
		return api.Agent{}, fmt.Errorf("%w: project lifecycle generation changed; refresh before admission", api.ErrConflict)
	}
	if req.Role != "" && req.Role != api.AgentRoleDatabaseHandler {
		return api.Agent{}, api.ErrInvalid
	}
	if req.Role == api.AgentRoleDatabaseHandler && req.ParentAgentID != "" {
		return api.Agent{}, api.ErrInvalid
	}
	if req.Role == api.AgentRoleDatabaseHandler && req.WorkItem != nil {
		return api.Agent{}, api.ErrInvalid
	}
	if req.Role == api.AgentRoleDatabaseHandler && !api.ValidID(req.AgentID, "agt") {
		return api.Agent{}, api.ErrInvalid
	}
	if req.ExpectedRunID != "" && ((resumeAdmission == nil && req.Role != api.AgentRoleDatabaseHandler) || !api.ValidID(req.AgentID, "agt") || !validRunID(req.ExpectedRunID)) {
		return api.Agent{}, api.ErrInvalid
	}
	if req.ParentAgentID != "" {
		parent, err := s.GetAgent(ctx, req.ParentAgentID)
		if err != nil || parent.TaskID != taskID || parent.Status == api.AgentClosed || parent.Status == api.AgentExited || parent.Status == api.AgentRetired {
			return api.Agent{}, api.ErrInvalid
		}
		// AllowAgentSpawn gates extra-helper launches and unbound parented
		// requests (checked below, once this request's team-role
		// classification is known); a regular team member bound to a work
		// item (TeamRoleMember) is not an "extra" and may be admitted even
		// while extra spawning is disabled for the task.
	}
	if req.WorkItem != nil {
		if err := validatePreparedContextBundle(req.WorkItem); err != nil {
			return api.Agent{}, err
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
			if exact, retryErr := exactExistingQueueAdmission(s.db, ctx, taskID, req, existing); retryErr != nil {
				return api.Agent{}, retryErr
			} else if exact {
				return existing, nil
			}
			if existing.WorkItem != nil || (req.WorkItem != nil && req.Role == "") {
				return api.Agent{}, fmt.Errorf("%w: item-bound agents require a fresh name and identity", api.ErrConflict)
			}
			if (existing.Role == api.AgentRoleDatabaseHandler || req.Role == api.AgentRoleDatabaseHandler) &&
				(existing.Name != req.Name || existing.Host != req.Host || existing.Session != req.Session || existing.Runtime != req.Runtime || existing.Cwd != req.Cwd || existing.ParentAgentID != req.ParentAgentID || existing.Role != req.Role) {
				return api.Agent{}, fmt.Errorf("%w: database handler launch settings changed", api.ErrConflict)
			}
			if existing.Role == api.AgentRoleDatabaseHandler {
				if existing.Status == api.AgentClosed {
					return api.Agent{}, api.ErrClosed
				}
				if req.ExpectedRunID != "" && req.ExpectedRunID != existing.RunID {
					return api.Agent{}, fmt.Errorf("%w: database handler run changed; refresh before restarting", api.ErrConflict)
				}
				if existing.Status == api.AgentExited {
					if req.ExpectedRunID == "" {
						return api.Agent{}, fmt.Errorf("%w: exited database handler requires expectedRunId", api.ErrConflict)
					}
					var active int
					if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE task_id=? AND status NOT IN ('closed','exited')`, taskID).Scan(&active); err != nil {
						return api.Agent{}, err
					}
					if active >= s.MaxAgents {
						return api.Agent{}, api.ErrLimit
					}
					runID := api.NewID("run")
					tx, err := s.db.BeginTx(ctx, nil)
					if err != nil {
						return api.Agent{}, err
					}
					defer tx.Rollback()
					result, err := tx.ExecContext(ctx, `UPDATE agents SET run_id=?,status='starting',last_seen_at='',cleanup_done=0,cleanup_error='' WHERE id=? AND run_id=? AND status='exited'`, runID, existing.ID, req.ExpectedRunID)
					if err != nil {
						return api.Agent{}, err
					}
					changed, err := result.RowsAffected()
					if err != nil {
						return api.Agent{}, err
					}
					if changed != 1 {
						return api.Agent{}, fmt.Errorf("%w: database handler run changed during restart", api.ErrConflict)
					}
					if _, err = s.insertEvent(ctx, tx, taskID, api.EventAgentAdded, existing.ID, existing.Name, map[string]any{"runId": runID, "role": existing.Role}, by); err != nil {
						return api.Agent{}, err
					}
					if err = tx.Commit(); err != nil {
						return api.Agent{}, err
					}
					s.notify(taskID)
					return s.GetAgent(ctx, existing.ID)
				}
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
		if req.Role == api.AgentRoleDatabaseHandler {
			return api.Agent{}, fmt.Errorf("%w: database handler restart requires its stable agentId and expectedRunId", api.ErrConflict)
		}
		if req.WorkItem != nil {
			return api.Agent{}, fmt.Errorf("%w: item-bound replacement requires a new agent name and identity", api.ErrConflict)
		}
		var priorBindings int
		if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_work_item_bindings WHERE agent_id=?`, previousID).Scan(&priorBindings); err != nil {
			return api.Agent{}, err
		}
		if priorBindings > 0 {
			return api.Agent{}, fmt.Errorf("%w: item-bound agents cannot restart into an unscoped session; create a fresh replacement", api.ErrConflict)
		}
		if previous.Status != api.AgentExited || previous.Host != req.Host || previous.ParentAgentID != req.ParentAgentID || previous.Role != req.Role {
			return api.Agent{}, api.ErrLimit
		}
		runID := api.NewID("run")
		_, err = s.db.ExecContext(ctx, `UPDATE agents SET session=?,runtime=?,cwd=?,run_id=?,status='starting',last_seen_at='',cleanup_done=0,cleanup_error='' WHERE id=?`, req.Session, req.Runtime, req.Cwd, runID, previousID)
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
	if req.Role == api.AgentRoleDatabaseHandler {
		var existingID string
		err = s.db.QueryRowContext(ctx, `SELECT id FROM agents WHERE task_id=? AND role=? AND status<>'closed' LIMIT 1`, taskID, api.AgentRoleDatabaseHandler).Scan(&existingID)
		if err == nil {
			return api.Agent{}, fmt.Errorf("%w: project already has a database handler", api.ErrConflict)
		}
		if err != sql.ErrNoRows {
			return api.Agent{}, err
		}
	}
	if req.ParentAgentID != "" && req.WorkItem == nil {
		// A parented agent with no work-item binding has no item to
		// classify a builder-vs-extra membership against, and no explicit
		// declaration is possible for it (TeamRole lives on the work-item
		// request). This is a known, disclosed limitation of the extra
		// allowance model, not silently resolved: such requests fall back
		// to task-wide accounting under AllowAgentSpawn, excluding closed
		// rows so a closed one frees its slot. It cannot benefit from
		// per-item isolation. Prefer a work-item-bound, explicitly
		// classified launch whenever one is possible.
		if !t.AllowAgentSpawn {
			return api.Agent{}, api.ErrAgentSpawnDisabled
		}
		var existing int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents a
WHERE a.task_id=? AND a.parent_agent_id<>'' AND a.status<>'closed'
AND NOT EXISTS (SELECT 1 FROM agent_work_item_bindings b WHERE b.agent_id=a.id)`,
			taskID).Scan(&existing); err != nil {
			return api.Agent{}, err
		}
		if existing >= t.MaxNewAgents {
			return api.Agent{}, api.ErrAgentSpawnLimit
		}
	}
	now := s.now()
	a := api.Agent{ID: req.AgentID, TaskID: taskID, Name: req.Name, Host: req.Host, Session: req.Session, Runtime: req.Runtime,
		Cwd: req.Cwd, ParentAgentID: req.ParentAgentID, Role: req.Role, Status: api.AgentStarting, CreatedAt: now, LastEventAt: now}
	if a.ID == "" {
		a.ID = api.NewID("agt")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	contextThrough, resolvedTeamRole, err := validateAgentWorkItemRequest(tx, ctx, taskID, req.ParentAgentID, req.WorkItem)
	if err != nil {
		return a, err
	}
	freshParentedItemBound := req.WorkItem != nil && req.ParentAgentID != "" && req.WorkItem.ReplacesAgentID == ""
	var intent *api.AllocationIntent
	if resumeAdmission != nil {
		a.RunID = resumeAdmission.RunID
	} else if freshParentedItemBound {
		// Independent review #2300/#2771/#2840/#2916 finding 5: a fresh
		// parented member OR extra admission must be authorized by a
		// durable, handler/lead-authored allocation intent recorded BEFORE
		// this call, bound to the exact preallocated agent identity, target
		// task, item, revision, work-order message, prepared context,
		// declared team role and a preallocated expected run ID that
		// becomes this admission's actual run (not merely a generated one
		// consumed incidentally). Self-declaration on the launch request
		// alone is not sufficient; absence, any mismatch, or a legacy
		// (pre-#2916) intent whose new fields are empty is rejected, not
		// silently bypassed. A replacement is exempt: it inherits its
		// predecessor's already-authorized classification, not a fresh one.
		if req.AgentID == "" {
			return a, fmt.Errorf("%w: a fresh parented member/extra admission requires a preallocated --agent-id bound to a recorded allocation intent", api.ErrConflict)
		}
		loaded, ierr := loadAllocationIntent(tx, ctx, req.AgentID)
		if ierr != nil {
			return a, ierr
		}
		if loaded == nil || loaded.ConsumedAt != nil || loaded.InvalidatedAt != nil {
			return a, fmt.Errorf("%w: no unconsumed allocation intent recorded for this agent identity", api.ErrConflict)
		}
		if loaded.TargetTaskID == "" || loaded.ContextDigest == "" || loaded.AuthorAgentID == "" || loaded.AuthorRunID == "" || loaded.ExpectedRunID == "" ||
			loaded.ExpectedLauncherAgentID == "" || loaded.ExpectedLauncherRunID == "" {
			return a, fmt.Errorf("%w: allocation intent predates the required expected-run/context/author/launcher binding and cannot authorize admission", api.ErrConflict)
		}
		contextDigestBytes := sha256.Sum256(req.WorkItem.ContextBundle)
		contextDigest := hex.EncodeToString(contextDigestBytes[:])
		if loaded.TargetTaskID != taskID || loaded.ItemTaskID != req.WorkItem.ItemTaskID || loaded.ItemID != req.WorkItem.ItemID ||
			loaded.ItemRevision != req.WorkItem.ItemRevision || loaded.WorkOrderMessage != req.WorkItem.WorkOrderMessage ||
			loaded.ContextDigest != contextDigest || loaded.TeamRole != resolvedTeamRole {
			return a, fmt.Errorf("%w: recorded allocation intent does not match this admission's exact target task/item/revision/order/context/team role", api.ErrConflict)
		}
		// Independent review #3003/#3010 finding 1: the actual launcher
		// performing this admission (ParentAgentID and its live run) must
		// match the intent's expected launcher exactly -- not merely be
		// recorded after the fact once it has already consumed the intent.
		// Any other active agent on the task holding the preallocated
		// --agent-id could otherwise consume an intent it was never
		// authorized to launch.
		var actualLauncherRunID string
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE id=?`, req.ParentAgentID).Scan(&actualLauncherRunID); err != nil {
			return a, err
		}
		if loaded.ExpectedLauncherAgentID != req.ParentAgentID || loaded.ExpectedLauncherRunID != actualLauncherRunID {
			return a, fmt.Errorf("%w: this launcher is not the one authorized by the recorded allocation intent", api.ErrConflict)
		}
		intent = loaded
		a.RunID = intent.ExpectedRunID
	} else {
		a.RunID = api.NewID("run")
	}
	if req.WorkItem != nil && req.ParentAgentID != "" && resolvedTeamRole == api.TeamRoleExtra && req.WorkItem.ReplacesAgentID == "" {
		// Extra-agent capacity is accounted per work item, on top of that
		// item's allocated team member(s), per owner clarification
		// (#2045/#2048): the "extra" allowance belongs to a bug/feature,
		// not the project as a whole, and closing an extra frees its slot.
		// Classification is the explicit resolvedTeamRole above (declared
		// by the caller, or inherited by a replacement) — never inferred
		// from binding/creation order — so a regular team member bound to
		// this item, however many already exist, never counts here. A
		// replacement (ReplacesAgentID set) is exempt from this gate
		// entirely: it continues the same already-reserved slot rather
		// than creating a new one, so it neither needs a free slot nor
		// consumes an extra one, and is not newly blocked by AllowAgentSpawn
		// being disabled after the original extra was admitted.
		// AllowAgentSpawn gates only this genuine fresh extra-helper case.
		if !t.AllowAgentSpawn {
			return a, api.ErrAgentSpawnDisabled
		}
		// A binding that has since been superseded by a replacement (some
		// other binding's replaces_agent_id points at it) must not be
		// double-counted alongside its replacement: they are one logical
		// continued slot, not two (independent review #2300/#2771 finding
		// 2). Only the live end of any replacement chain counts.
		//
		// A legacy parented binding predating this correction has
		// team_role='' -- unknown, not "member". Independent review
		// #2300/#2771 finding 4: excluding it from this count entirely
		// would silently refund the item's already-consumed allowance,
		// letting maxNewAgents fresh extras stack on top of open legacy
		// ones the owner's ceiling was meant to include. The conservative,
		// owner-ceiling-preserving choice is to count it as an extra until
		// an authorized migration/classification decision reclassifies it;
		// a parentless legacy binding is unambiguous (never an extra, then
		// or now) and is excluded exactly as before.
		var activeExtras int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents a
JOIN agent_work_item_bindings b ON b.agent_id=a.id
WHERE a.task_id=? AND a.status<>'closed' AND b.item_task_id=? AND b.item_id=?
AND (b.team_role=? OR (b.team_role='' AND a.parent_agent_id<>''))
AND NOT EXISTS (SELECT 1 FROM agent_work_item_bindings r JOIN agents ra ON ra.id=r.agent_id WHERE r.replaces_agent_id=b.agent_id AND ra.status<>'closed')`,
			taskID, req.WorkItem.ItemTaskID, req.WorkItem.ItemID, api.TeamRoleExtra).Scan(&activeExtras); err != nil {
			return a, err
		}
		if activeExtras >= t.MaxNewAgents {
			return a, api.ErrAgentSpawnLimit
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.TaskID, a.Name, a.Host, a.Session, a.Runtime, a.Cwd, a.ParentAgentID, a.Role, a.Status, a.Title, ts(now), ts(now), a.RunID, "", "", "", false, "")
	if err != nil {
		return a, err
	}
	if a.WorkItem, err = insertAgentWorkItemBinding(ctx, tx, a, req.WorkItem, contextThrough, resolvedTeamRole); err != nil {
		return a, err
	}
	if freshParentedItemBound {
		// Consume the intent atomically in this same transaction: it
		// authorizes exactly this one fresh admission, never a later one.
		// Launcher identity (the actual ParentAgentID performing this
		// spawn) is recorded separately from author identity so authorship
		// and launch provenance remain distinguishable even when an author
		// delegates the actual launch to a different agent.
		if _, err = tx.ExecContext(ctx, `UPDATE agent_allocation_intents SET consumed_at=?, consumed_by_run_id=?, launcher_agent_id=?, launcher_run_id=(SELECT run_id FROM agents WHERE id=?) WHERE agent_id=?`,
			ts(now), a.RunID, req.ParentAgentID, req.ParentAgentID, req.AgentID); err != nil {
			return a, err
		}
	}
	if err = s.pinCrossProjectQueueAdmission(ctx, tx, taskID, req.WorkItem, a); err != nil {
		return a, err
	}
	if a.WorkItem != nil {
		a.ReadUpTo = contextThrough
	}
	eventData := map[string]any{"host": a.Host, "session": a.Session, "runtime": a.Runtime, "parentAgentId": a.ParentAgentID, "role": a.Role, "runId": a.RunID}
	if a.WorkItem != nil {
		eventData["workItem"] = a.WorkItem
	}
	if _, err = s.insertEvent(ctx, tx, taskID, api.EventAgentAdded, a.ID, a.Name, eventData, by); err != nil {
		return a, err
	}
	if err = tx.Commit(); err != nil {
		return a, err
	}
	s.notify(taskID)
	return a, nil
}

func (s *Store) GetAgent(ctx context.Context, id string) (api.Agent, error) {
	a, err := scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, api.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if err = s.loadAgentWorkItem(ctx, &a); err != nil {
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
		if err = s.loadAgentWorkItem(ctx, &out[i]); err != nil {
			return nil, err
		}
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
		if task.PauseState != api.ProjectPauseActive && *req.Status != api.AgentClosed && *req.Status != api.AgentExited {
			return a, fmt.Errorf("%w: agent lifecycle reactivation is blocked while the project is paused", api.ErrConflict)
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
	if err := api.NormalizeEnvelopePost(&req); err != nil {
		return api.Message{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || validateMessageRequestShape(req) != nil {
		return api.Message{}, api.ErrInvalid
	}
	payload := ""
	if req.RequestID != "" {
		payload = requestHash(req)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()
	if req.RequestID != "" {
		receipt, priorHash, receiptErr := findMessagePostReceipt(tx, ctx, taskID, req.RequestID, req.AgentID, by)
		if receiptErr == nil {
			if priorHash != payload {
				return api.Message{}, workItemConflict("request ID was already used with different message data")
			}
			message, loadErr := loadMessage(tx, ctx, taskID, receipt.MessageSeq)
			if loadErr != nil {
				return message, loadErr
			}
			message.PostReceipt = &receipt
			return message, nil
		}
		if !errors.Is(receiptErr, api.ErrNotFound) {
			return api.Message{}, receiptErr
		}
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, api.ErrNotFound
	}
	if err != nil {
		return api.Message{}, err
	}
	if t.Status != api.TaskOpen {
		return api.Message{}, api.ErrClosed
	}
	if req.Text == "" || !api.ValidText(req.Text, api.MaxTextLen) || req.ReplyTo < 0 {
		return api.Message{}, api.ErrInvalid
	}
	var target api.Agent
	for _, id := range []string{req.To, req.AgentID} {
		if id == "" {
			continue
		}
		a, agentErr := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, id))
		if agentErr != nil || a.TaskID != taskID {
			return api.Message{}, api.ErrInvalid
		}
		if id == req.To {
			target = a
		}
	}
	// A typed message's stated recipient must be the one it is routed to.
	if e := req.Envelope; e != nil && e.To != "" && !strings.HasPrefix(e.To, "role:") && (req.To == "" || (target.ID != e.To && target.Name != e.To)) {
		return api.Message{}, &api.EnvelopeError{Problems: []api.Problem{{Field: "to", Reason: "must name the agent the message is addressed to"}}}
	}
	if req.ReplyTo > 0 {
		var replyTask string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM messages WHERE seq=?`, req.ReplyTo).Scan(&replyTask); err != nil || replyTask != taskID {
			return api.Message{}, api.ErrInvalid
		}
	}
	m, err := s.insertMessage(ctx, tx, t, req, target, by, false, false)
	if err != nil {
		return m, err
	}
	// Broker phase 2a: only posts through this public endpoint create or settle
	// obligations. System notices, decisions, dispatches and lead notices use
	// other insert paths and never oblige anyone.
	if err = s.createObligations(ctx, tx, m, req, req.AgentID == "" && by.Node != api.BrokerNode); err != nil {
		return m, err
	}
	if err = s.applyReplyOutcome(ctx, tx, m, req); err != nil {
		return m, err
	}
	if req.RequestID != "" {
		if err = insertMessagePostReceipt(ctx, tx, &m, req.RequestID, payload, by); err != nil {
			return m, err
		}
	}
	if err = tx.Commit(); err != nil {
		return m, err
	}
	s.notify(taskID)
	return m, nil
}

func (s *Store) insertMessage(ctx context.Context, tx *sql.Tx, task api.Task, req api.PostMessageRequest, target api.Agent, by api.Caller, allowCrossProject, allowHistoricalRevision bool) (api.Message, error) {
	return s.insertMessageWithResume(ctx, tx, task, req, target, by, allowCrossProject, allowHistoricalRevision, true)
}

// insertMessageWithResume keeps notification-only internal callers out of the
// explicit human resumption path. This policy is not exposed on the wire.
func (s *Store) insertMessageWithResume(ctx context.Context, tx *sql.Tx, task api.Task, req api.PostMessageRequest, target api.Agent, by api.Caller, allowCrossProject, allowHistoricalRevision, allowResume bool) (api.Message, error) {
	taskID := task.ID
	// Every insert path enforces the envelope, not only PostMessage.
	if err := api.NormalizeEnvelopePost(&req); err != nil {
		return api.Message{}, err
	}
	if err := validateMessageContext(tx, ctx, taskID, req, allowCrossProject, allowHistoricalRevision); err != nil {
		return api.Message{}, err
	}
	m := api.Message{Broadcast: task.Swarm, ReplyTo: req.ReplyTo, TaskID: taskID, From: api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}, To: req.To, Text: req.Text, Envelope: req.Envelope, CreatedAt: s.now()}
	envelope := ""
	if req.Envelope != nil {
		raw, err := json.Marshal(req.Envelope)
		if err != nil {
			return m, err
		}
		envelope = string(raw)
	}
	fromRun := ""
	if req.AgentID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE id=?`, req.AgentID).Scan(&fromRun); err != nil {
			return m, err
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO messages (task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast,from_run_id,envelope) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		taskID, req.AgentID, by.Node, by.User, req.To, req.Text, ts(m.CreatedAt), req.ReplyTo, m.Broadcast, fromRun, envelope)
	if err != nil {
		return m, err
	}
	m.Seq, _ = res.LastInsertId()
	if err = insertMessageContext(ctx, tx, &m, req, allowCrossProject); err != nil {
		return m, err
	}
	if err = s.insertMessageCheck(ctx, tx, m, req); err != nil {
		return m, err
	}

	preview := req.Text
	if len(preview) > 200 {
		preview = preview[:200]
	}
	eventData := map[string]any{"seq": m.Seq, "to": req.To, "broadcast": m.Broadcast}
	if auditKind := auditClassificationForPost(req); auditKind != api.MessageAuditUnclassified {
		eventData["auditKind"] = auditKind
	}
	if len(m.WorkItems) > 0 {
		eventData["workItems"] = m.WorkItems
	}
	if m.WorkOrderMessage != nil {
		eventData["workOrderMessage"] = m.WorkOrderMessage
	}
	if _, err = s.insertEvent(ctx, tx, taskID, api.EventMessage, req.AgentID, preview, eventData, by); err != nil {
		return m, err
	}
	// A direct human message is an explicit request to continue an existing
	// retired run. The heartbeat-derived Online flag prevents recreating or
	// waking stale sessions; agent-authored and unaddressed messages do not resume.
	if allowResume && task.PauseState == api.ProjectPauseActive && req.AgentID == "" && req.To != "" && target.Status == api.AgentRetired && target.Online {
		now := s.now()
		result, err := tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=? AND task_id=? AND status=?`,
			api.AgentDone, ts(now), target.ID, taskID, api.AgentRetired)
		if err != nil {
			return m, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return m, err
		}
		if changed == 1 {
			if _, err = s.insertEvent(ctx, tx, taskID, api.EventResumed, target.ID, "", nil, by); err != nil {
				return m, err
			}
		}
	}
	return m, nil
}

// ListMessages returns messages after seq. When agentID is set, only broadcast
// messages and messages addressed to that agent are returned.
func (s *Store) ListMessages(ctx context.Context, taskID string, after int64, agentID string, limit int) ([]api.Message, error) {
	if limit <= 0 || limit > api.MaxLimit {
		limit = api.MaxLimit
	}
	var binding *api.AgentWorkItemBinding
	if agentID != "" {
		var runID, agentTaskID string
		if err := s.db.QueryRowContext(ctx, `SELECT run_id,task_id FROM agents WHERE id=?`, agentID).Scan(&runID, &agentTaskID); err != nil || agentTaskID != taskID {
			return nil, api.ErrInvalid
		}
		var err error
		binding, err = loadAgentWorkItemBinding(s.db, ctx, agentID, runID)
		if err != nil {
			return nil, err
		}
	}
	q := `SELECT ` + messageSelectCols + ` FROM messages m
` + messageSelectJoins
	args := []any{}
	if binding != nil {
		q += `
LEFT JOIN message_work_item_links item_scope ON item_scope.message_seq=m.seq AND item_scope.item_task_id=? AND item_scope.item_id=?`
		args = append(args, binding.ItemTaskID, binding.ItemID)
	}
	q += `
WHERE m.task_id=? AND m.seq>?`
	args = append(args, taskID, after)
	if agentID != "" {
		q += ` AND (broadcast=1 OR to_agent='' OR to_agent=?)`
		args = append(args, agentID)
	}
	if binding != nil {
		q += ` AND (item_scope.message_seq IS NOT NULL OR (m.system_notice_kind='queue_changed' AND m.to_agent=?))`
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
	out := []api.Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range out {
		if err := loadSystemNotice(s.db, ctx, &out[index]); err != nil {
			return nil, err
		}
		original, err := loadAuditOriginal(s.db, ctx, out[index].TaskID, out[index].Seq)
		if err != nil {
			return nil, err
		}
		if original.Classification != api.MessageAuditUnclassified {
			out[index].WorkItems = append([]api.MessageWorkItem(nil), original.WorkItems...)
			out[index].WorkOrderMessage = original.WorkOrderMessage
		}
	}
	if after < 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
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
	var runID string
	if err = s.db.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE id=? AND task_id=?`, agentID, taskID).Scan(&runID); err != nil {
		return 0, err
	}
	binding, err := loadAgentWorkItemBinding(s.db, ctx, agentID, runID)
	if err != nil {
		return 0, err
	}
	query := `SELECT COUNT(*) FROM messages m`
	args := []any{}
	if binding != nil {
		query += ` JOIN message_work_item_links scope ON scope.message_seq=m.seq AND scope.item_task_id=? AND scope.item_id=?`
		args = append(args, binding.ItemTaskID, binding.ItemID)
	}
	query += ` WHERE m.task_id=? AND m.seq>? AND m.from_agent<>? AND (m.broadcast=1 OR m.to_agent='' OR m.to_agent=?)`
	args = append(args, taskID, cursor, agentID, agentID)
	var n int
	err = s.db.QueryRowContext(ctx, query, args...).Scan(&n)
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
	e, err := s.insertEvent(ctx, s.db, taskID, kind, agentID, text, data, by)
	if err == nil {
		s.notify(taskID)
	}
	return e, err
}

type eventExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) insertEvent(ctx context.Context, execer eventExecer, taskID, kind, agentID, text string, data map[string]any, by api.Caller) (api.Event, error) {
	e := api.Event{TaskID: taskID, Kind: kind, AgentID: agentID, Text: text, Data: data, By: by, CreatedAt: s.now()}
	encoded := ""
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return e, err
		}
		encoded = string(b)
	}
	res, err := execer.ExecContext(ctx, `INSERT INTO events (task_id,kind,agent_id,text,data,by_node,by_user,created_at) VALUES (?,?,?,?,?,?,?,?)`,
		taskID, kind, agentID, text, encoded, by.Node, by.User, ts(e.CreatedAt))
	if err != nil {
		return e, err
	}
	e.Seq, _ = res.LastInsertId()
	if agentID != "" {
		_, _ = execer.ExecContext(ctx, `UPDATE agents SET last_event_at=? WHERE id=?`, ts(e.CreatedAt), agentID)
	}
	_, _ = execer.ExecContext(ctx, `DELETE FROM events WHERE task_id=? AND seq <= (SELECT seq FROM events WHERE task_id=? ORDER BY seq DESC LIMIT 1 OFFSET ?)`,
		taskID, taskID, api.MaxEventsPerTask)
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
