package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const messageChecksSchema = `
CREATE TABLE IF NOT EXISTS message_checks (
  seq INTEGER PRIMARY KEY REFERENCES messages(seq),
  task_id TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  form TEXT NOT NULL,
  problems TEXT NOT NULL DEFAULT '[]',
  jev_status TEXT NOT NULL,
  jev TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  scored_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS message_checks_task ON message_checks(task_id, seq);
CREATE INDEX IF NOT EXISTS message_checks_jev ON message_checks(jev_status, seq);`

// EnableJevScoring makes new agent posts wait for the Jev scorer instead of
// recording it as disabled. Call it only when a scorer is running.
func (s *Store) EnableJevScoring() { s.jevEnabled.Store(true) }

// classifyMessage decides a message's shadow-mode form without changing
// whether it is accepted.
func classifyMessage(req api.PostMessageRequest) (form string, problems []api.Problem) {
	switch {
	case req.AgentID == "":
		return api.CheckFormHuman, nil
	case req.Envelope != nil:
		return api.CheckFormTyped, nil
	}
	parsed, matched := api.ParseTextConvention(req.Text)
	if !matched {
		return api.CheckFormFreeText, nil
	}
	if problems = api.ValidateEnvelope(parsed); problems != nil {
		return api.CheckFormTextConventionInvalid, problems
	}
	return api.CheckFormTextConvention, nil
}

// insertMessageCheck runs inside the message's own transaction, so every
// committed message has exactly one check row.
func (s *Store) insertMessageCheck(ctx context.Context, tx *sql.Tx, m api.Message, req api.PostMessageRequest) error {
	form, problems := classifyMessage(req)
	jevStatus := api.JevStatusSkipped
	if form != api.CheckFormHuman {
		jevStatus = api.JevStatusDisabled
		if s.jevEnabled.Load() {
			jevStatus = api.JevStatusPending
		}
	}
	if problems == nil {
		problems = []api.Problem{}
	}
	raw, err := json.Marshal(problems)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO message_checks (seq,task_id,agent_id,form,problems,jev_status,created_at) VALUES (?,?,?,?,?,?,?)`,
		m.Seq, m.TaskID, req.AgentID, form, string(raw), jevStatus, ts(m.CreatedAt))
	return err
}

const messageCheckCols = `seq,task_id,agent_id,form,problems,jev_status,jev,created_at,scored_at`

func scanMessageCheck(row rowScanner) (api.MessageCheck, error) {
	var c api.MessageCheck
	var problems, jev, created, scored string
	if err := row.Scan(&c.Seq, &c.TaskID, &c.AgentID, &c.Form, &problems, &c.JevStatus, &jev, &created, &scored); err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(problems), &c.Problems); err != nil {
		return c, err
	}
	if jev != "" {
		c.Jev = &api.JevScores{}
		if err := json.Unmarshal([]byte(jev), c.Jev); err != nil {
			return c, err
		}
	}
	c.CreatedAt = parseTS(created)
	if scored != "" {
		t := parseTS(scored)
		c.ScoredAt = &t
	}
	return c, nil
}

// ListMessageChecks pages a task's check rows by message sequence.
func (s *Store) ListMessageChecks(ctx context.Context, taskID string, after int64, limit int) ([]api.MessageCheck, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageCheckCols+` FROM message_checks WHERE task_id=? AND seq>? ORDER BY seq LIMIT ?`, taskID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.MessageCheck{}
	for rows.Next() {
		c, err := scanMessageCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PendingJevCheck is one agent post waiting for Jev.
type PendingJevCheck struct {
	Seq      int64
	Text     string
	Envelope *api.Envelope
}

// PendingJevChecks returns the oldest pending posts. The scorer is the only
// consumer, so rows stay pending until scored and resume after a restart.
func (s *Store) PendingJevChecks(ctx context.Context, limit int) ([]PendingJevCheck, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.seq,m.text,m.envelope FROM message_checks c JOIN messages m ON m.seq=c.seq WHERE c.jev_status=? ORDER BY c.seq LIMIT ?`, api.JevStatusPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingJevCheck
	for rows.Next() {
		var p PendingJevCheck
		var envelope string
		if err := rows.Scan(&p.Seq, &p.Text, &envelope); err != nil {
			return nil, err
		}
		if envelope != "" {
			p.Envelope = &api.Envelope{}
			if err := json.Unmarshal([]byte(envelope), p.Envelope); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordJevResult stores a scorer outcome for a pending check.
func (s *Store) RecordJevResult(ctx context.Context, seq int64, status string, scores api.JevScores) error {
	raw, err := json.Marshal(scores)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.db.ExecContext(ctx, `UPDATE message_checks SET jev_status=?,jev=?,scored_at=? WHERE seq=? AND jev_status=?`,
		status, string(raw), ts(time.Now().UTC()), seq, api.JevStatusPending)
	return err
}
