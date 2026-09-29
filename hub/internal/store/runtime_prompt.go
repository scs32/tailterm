package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Runtime prompts (docs/runtime-prompts.md). The policy says what the relay
// does per prompt kind; escalations are remembered per agent run and prompt
// fingerprint so each prompt reaches a person once.
func migrateRuntimePrompt(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS runtime_prompt_policy (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id), actions TEXT NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runtime_prompt_escalations (
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, fingerprint TEXT NOT NULL, kind TEXT NOT NULL,
 created_at TEXT NOT NULL, PRIMARY KEY(agent_id,run_id,fingerprint));`)
	return err
}

func loadRuntimePromptPolicy(ctx context.Context, q queryRower, task string) (api.RuntimePromptPolicy, error) {
	p := api.RuntimePromptPolicy{TaskID: task, Actions: api.DefaultRuntimePromptActions()}
	var actions, updated string
	err := q.QueryRowContext(ctx, `SELECT actions,revision,updated_at FROM runtime_prompt_policy WHERE task_id=?`, task).Scan(&actions, &p.Revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	var saved map[string]string
	if err := json.Unmarshal([]byte(actions), &saved); err != nil {
		return p, fmt.Errorf("runtime prompt policy: %w", err)
	}
	// A saved action that is no longer allowed falls back to the default.
	for kind, action := range saved {
		if api.RuntimePromptActionAllowed(kind, action) {
			p.Actions[kind] = action
		}
	}
	t := parseTS(updated)
	p.UpdatedAt = &t
	return p, nil
}

func (s *Store) RuntimePromptPolicy(ctx context.Context, task string) (api.RuntimePromptPolicy, error) {
	if _, err := s.GetTask(ctx, task); err != nil {
		return api.RuntimePromptPolicy{}, err
	}
	return loadRuntimePromptPolicy(ctx, s.db, task)
}

// SetRuntimePromptPolicy is the owner's. The expected revision must match, and
// every listed action must be allowed for its kind.
func (s *Store) SetRuntimePromptPolicy(ctx context.Context, task string, req api.RuntimePromptPolicyRequest) (api.RuntimePromptPolicy, error) {
	var zero api.RuntimePromptPolicy
	if req.ActorAgentID != "" {
		return zero, api.ErrRuntimePromptOwnerOnly
	}
	if req.ExpectedRevision < 0 || len(req.Actions) == 0 {
		return zero, api.ErrInvalid
	}
	for kind, action := range req.Actions {
		if !api.RuntimePromptActionAllowed(kind, action) {
			return zero, fmt.Errorf("%w: %s cannot be %q", api.ErrInvalid, kind, action)
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	if t.Status != api.TaskOpen {
		return zero, api.ErrClosed
	}
	current, err := loadRuntimePromptPolicy(ctx, tx, task)
	if err != nil {
		return zero, err
	}
	if current.Revision != req.ExpectedRevision {
		return zero, fmt.Errorf("%w: runtime prompt policy is at revision %d; reload before changing it", api.ErrConflict, current.Revision)
	}
	for kind, action := range req.Actions {
		current.Actions[kind] = action
	}
	actions, _ := json.Marshal(current.Actions)
	now := s.now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO runtime_prompt_policy(task_id,actions,revision,updated_at) VALUES(?,?,?,?)
 ON CONFLICT(task_id) DO UPDATE SET actions=excluded.actions,revision=excluded.revision,updated_at=excluded.updated_at`,
		task, string(actions), current.Revision+1, ts(now)); err != nil {
		return zero, err
	}
	out := api.RuntimePromptPolicy{TaskID: task, Actions: current.Actions, Revision: current.Revision + 1, UpdatedAt: &now}
	if _, err = s.insertEvent(ctx, tx, task, "task_updated", "", "Runtime prompt policy saved", map[string]any{"runtimePromptPolicy": out}, api.Caller{Node: "workspace", User: "owner"}); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

// postRuntimePromptEscalation tells the owner, and the item lead when there
// is one, that an agent is waiting on a runtime prompt a person must answer.
// It posts once per agent run and prompt fingerprint: repeats, replays,
// prompt flaps and relay restarts find the existing row and post nothing.
func (s *Store) postRuntimePromptEscalation(ctx context.Context, tx *sql.Tx, task, agent, run string, activity api.AgentActivity) error {
	p := activity.Prompt
	if activity.State != "runtime_prompt" || p == nil || !api.RuntimePromptEscalates(p.Outcome) {
		return nil
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runtime_prompt_escalations(task_id,agent_id,run_id,fingerprint,kind,created_at) VALUES(?,?,?,?,?,?)`,
		task, agent, run, p.Fingerprint, p.Kind, ts(s.now()))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return err
	}
	var item, leadID, leadRun string
	err = tx.QueryRowContext(ctx, `SELECT item_id FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, agent, run).Scan(&item)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if item != "" {
		err = tx.QueryRowContext(ctx, `SELECT agent_id,run_id FROM item_team_leads WHERE task_id=? AND item_id=? AND state<>'closed'`, task, item).Scan(&leadID, &leadRun)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	project, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE tasks.id=?`, task))
	if err != nil {
		return err
	}
	name := agent
	if a, loadErr := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, agent)); loadErr == nil && a.Name != "" {
		name = a.Name
	}
	refs := map[string]string{"agent": agent, "run": run, "activity": activity.State, "promptKind": p.Kind}
	if item != "" {
		refs["item"] = item
	}
	text := fmt.Sprintf("%s is waiting on a runtime prompt: %s (since %s). Policy action %s, outcome %s.", name, p.Label, p.Since.UTC().Format(time.RFC3339), p.Action, p.Outcome)
	if p.Reason != "" {
		text += " " + p.Reason + "."
	}
	text += " Open the agent's terminal to answer it; the relay will not type into it."
	subject := "Agent is waiting on a runtime prompt"
	if err := s.postBrokerNotice(ctx, tx, project, api.Agent{}, "", subject, subject, text, refs); err != nil {
		return err
	}
	if leadID != "" && leadID != agent {
		lead, loadErr := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND run_id=?`, leadID, leadRun))
		if loadErr == nil && lead.Status != api.AgentClosed && lead.Status != api.AgentExited {
			if err := s.postBrokerNotice(ctx, tx, project, lead, lead.Name, subject, subject, text, refs); err != nil {
				return err
			}
		}
	}
	return nil
}
