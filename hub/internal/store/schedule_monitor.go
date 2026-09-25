package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// ScheduleMonitorNotice is the durable delivery state for one detected stall.
// The scheduler owns these records; they are deliberately separate from Queue
// state and never authorize a Queue or agent lifecycle mutation.
type ScheduleMonitorNotice struct {
	TaskID            string
	Fingerprint       string
	FirstDetectedAt   time.Time
	LastObservedAt    time.Time
	LastAttemptAt     time.Time
	LastNotifiedAt    time.Time
	NotificationCount int
	FailureCount      int
	MessageSeq        int64
	LastError         string
	RecipientID       string
	RecipientRunID    string
	PendingRequestID  string
}

func migrateScheduleMonitor(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schedule_monitor_notices (
  task_id TEXT NOT NULL REFERENCES tasks(id),
  fingerprint TEXT NOT NULL,
  first_detected_at TEXT NOT NULL,
  last_observed_at TEXT NOT NULL,
  last_notified_at TEXT NOT NULL DEFAULT '',
  notification_count INTEGER NOT NULL DEFAULT 0,
  message_seq INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(task_id,fingerprint)
);`)
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"last_attempt_at", "TEXT NOT NULL DEFAULT ''"},
		{"failure_count", "INTEGER NOT NULL DEFAULT 0"},
		{"recipient_id", "TEXT NOT NULL DEFAULT ''"},
		{"recipient_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"pending_request_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('schedule_monitor_notices') WHERE name=?`, column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := db.Exec("ALTER TABLE schedule_monitor_notices ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS schedule_monitor_generation (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1), next_generation INTEGER NOT NULL CHECK(next_generation>0));
 INSERT OR IGNORE INTO schedule_monitor_generation(singleton,next_generation) VALUES(1,1);
 UPDATE schedule_monitor_notices SET last_attempt_at=last_notified_at
 WHERE last_attempt_at='' AND last_notified_at<>'';`)
	return err
}

func (s *Store) GetScheduleMonitorNotice(ctx context.Context, taskID, fingerprint string) (ScheduleMonitorNotice, error) {
	if !api.ValidID(taskID, "tsk") || fingerprint == "" {
		return ScheduleMonitorNotice{}, api.ErrInvalid
	}
	return loadScheduleMonitorNotice(ctx, s.db, taskID, fingerprint)
}

func loadScheduleMonitorNotice(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, taskID, fingerprint string) (ScheduleMonitorNotice, error) {
	var notice ScheduleMonitorNotice
	var first, observed, attempted, notified string
	err := db.QueryRowContext(ctx, `SELECT task_id,fingerprint,first_detected_at,last_observed_at,last_attempt_at,last_notified_at,notification_count,failure_count,message_seq,last_error,recipient_id,recipient_run_id,pending_request_id
FROM schedule_monitor_notices WHERE task_id=? AND fingerprint=?`, taskID, fingerprint).Scan(
		&notice.TaskID, &notice.Fingerprint, &first, &observed, &attempted, &notified, &notice.NotificationCount, &notice.FailureCount, &notice.MessageSeq, &notice.LastError, &notice.RecipientID, &notice.RecipientRunID, &notice.PendingRequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return notice, api.ErrNotFound
	}
	notice.FirstDetectedAt, notice.LastObservedAt = parseTS(first), parseTS(observed)
	if attempted != "" {
		notice.LastAttemptAt = parseTS(attempted)
	}
	if notified != "" {
		notice.LastNotifiedAt = parseTS(notified)
	}
	return notice, err
}

// SaveScheduleMonitorNotice records observation and successful/failed delivery.
// It intentionally does not create events, alter Queue entries, or touch agents.
func (s *Store) SaveScheduleMonitorNotice(ctx context.Context, notice ScheduleMonitorNotice) error {
	if !api.ValidID(notice.TaskID, "tsk") || notice.Fingerprint == "" || notice.FirstDetectedAt.IsZero() || notice.LastObservedAt.IsZero() || notice.NotificationCount < 0 || notice.FailureCount < 0 || notice.MessageSeq < 0 || !api.ValidText(notice.LastError, api.MaxTextLen) {
		return api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return saveScheduleMonitorNotice(ctx, s.db, notice)
}

func saveScheduleMonitorNotice(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, notice ScheduleMonitorNotice) error {
	_, err := db.ExecContext(ctx, `INSERT INTO schedule_monitor_notices
(task_id,fingerprint,first_detected_at,last_observed_at,last_attempt_at,last_notified_at,notification_count,failure_count,message_seq,last_error,recipient_id,recipient_run_id,pending_request_id)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(task_id,fingerprint) DO UPDATE SET
last_observed_at=excluded.last_observed_at,last_attempt_at=excluded.last_attempt_at,last_notified_at=excluded.last_notified_at,
notification_count=excluded.notification_count,failure_count=excluded.failure_count,message_seq=excluded.message_seq,last_error=excluded.last_error,
recipient_id=excluded.recipient_id,recipient_run_id=excluded.recipient_run_id,pending_request_id=excluded.pending_request_id`,
		notice.TaskID, notice.Fingerprint, ts(notice.FirstDetectedAt), ts(notice.LastObservedAt),
		func() string {
			if notice.LastAttemptAt.IsZero() {
				return ""
			}
			return ts(notice.LastAttemptAt)
		}(),
		func() string {
			if notice.LastNotifiedAt.IsZero() {
				return ""
			}
			return ts(notice.LastNotifiedAt)
		}(),
		notice.NotificationCount, notice.FailureCount, notice.MessageSeq, notice.LastError, notice.RecipientID, notice.RecipientRunID, notice.PendingRequestID)
	return err
}

// PruneScheduleMonitorNotices bounds stale fingerprints. It removes only
// scheduler-local delivery metadata, never Queue, messages, agent state, or audit history.
func (s *Store) PruneScheduleMonitorNotices(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `DELETE FROM schedule_monitor_notices WHERE last_observed_at<?`, ts(before))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ScheduleMonitorDelivery serializes the final lead check, delivery receipt and
// success state with lifecycle writes. No public posting or lifecycle API is used.
// A delivery failure commits only retry metadata; the savepoint rolls back any
// partial message/event/receipt. A failed outer commit cannot leave orphan mail.
func (s *Store) ScheduleMonitorDelivery(ctx context.Context, taskID string, lead api.Agent, fingerprint, text string, now time.Time, initial, maximum time.Duration) (ScheduleMonitorNotice, string, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(lead.ID, "agt") || lead.RunID == "" || fingerprint == "" || text == "" || !api.ValidText(text, api.MaxTextLen) || now.IsZero() || initial <= 0 || maximum < initial {
		return ScheduleMonitorNotice{}, "unknown", api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduleMonitorNotice{}, "unknown", err
	}
	defer tx.Rollback()
	notice, err := loadScheduleMonitorNotice(ctx, tx, taskID, fingerprint)
	if errors.Is(err, api.ErrNotFound) {
		notice = ScheduleMonitorNotice{TaskID: taskID, Fingerprint: fingerprint, FirstDetectedAt: now}
	} else if err != nil {
		return notice, "unknown", err
	}
	if now.After(notice.LastObservedAt) {
		notice.LastObservedAt = now
	}
	finish := func(state string, deliveryErr error) (ScheduleMonitorNotice, string, error) {
		if err := saveScheduleMonitorNotice(ctx, tx, notice); err != nil {
			return notice, "unknown", err
		}
		if err := tx.Commit(); err != nil {
			return notice, "unknown", err
		}
		if state == "notified" {
			s.notify(taskID)
		}
		return notice, state, deliveryErr
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return notice, "unknown", err
	}
	var target api.Agent
	var deliveryErr error
	if task.Status != api.TaskOpen || task.PauseState != api.ProjectPauseActive {
		deliveryErr = api.ErrClosed
	} else {
		target, err = scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, lead.ID))
		if err != nil {
			return notice, "unknown", err
		}
		var matches int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM item_team_leads WHERE task_id=? AND agent_id=? AND run_id=? AND state='running'`, taskID, lead.ID, lead.RunID).Scan(&matches); err != nil {
			return notice, "unknown", err
		}
		var limit int
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, taskID).Scan(&limit); err != nil {
			return notice, "unknown", err
		}
		legacyLead := limit == 1 && task.Orchestrator != "" && target.Name == task.Orchestrator
		if (matches != 1 && !legacyLead) || target.TaskID != taskID || target.Role != "" || target.RunID != lead.RunID {
			return finish("unknown", fmt.Errorf("orchestrator identity changed before delivery"))
		}
		switch target.Status {
		case api.AgentRetired, api.AgentNeedsInput, api.AgentClosed, api.AgentExited:
			return finish("intentional_block", nil)
		}
	}
	// Legacy rows have no pinned recipient. Retain their backoff, but never reuse
	// a legacy request ID. A known recipient/run change starts a new retry window.
	if notice.RecipientID != "" && (notice.RecipientID != lead.ID || notice.RecipientRunID != lead.RunID) {
		notice.PendingRequestID = ""
		notice.NotificationCount, notice.FailureCount, notice.MessageSeq = 0, 0, 0
		notice.LastAttemptAt, notice.LastNotifiedAt = time.Time{}, time.Time{}
		notice.LastError = ""
	}
	notice.RecipientID, notice.RecipientRunID = lead.ID, lead.RunID
	count := notice.NotificationCount
	if notice.FailureCount > 0 {
		count = notice.FailureCount
	}
	if !notice.LastAttemptAt.IsZero() && now.Sub(notice.LastAttemptAt) < ScheduleMonitorBackoff(initial, maximum, count) {
		return finish("suppressed", nil)
	}
	if notice.PendingRequestID == "" {
		var generation int64
		if err = tx.QueryRowContext(ctx, `UPDATE schedule_monitor_generation SET next_generation=next_generation+1 WHERE singleton=1 AND next_generation<9223372036854775807 RETURNING next_generation-1`).Scan(&generation); err != nil {
			return notice, "unknown", err
		}
		notice.PendingRequestID = fmt.Sprintf("schedule-monitor:v2:%d", generation)
	}
	notice.LastAttemptAt = now
	if deliveryErr == nil {
		if _, err = tx.ExecContext(ctx, `SAVEPOINT monitor_message`); err != nil {
			return notice, "unknown", err
		}
		// Broker phase 3: a typed notice, so readers (the Discord bridge,
		// Jev, the stop hook) see a hub notice, never an owner message.
		req := api.PostMessageRequest{To: target.ID, RequestID: notice.PendingRequestID, Envelope: &api.Envelope{
			Kind: api.EnvelopeKindNotice, To: target.Name, Subject: "Queue stall needs the lead's attention",
			Refs: map[string]string{"queue": "stall"}, Body: api.EnvelopeBody{Text: text}}}
		if err := api.NormalizeEnvelopePost(&req); err != nil {
			return notice, "unknown", err
		}
		by := api.Caller{Node: "system", User: "schedule-monitor"}
		message, postErr := s.insertMessageWithResume(ctx, tx, task, req, target, by, false, false, false)
		if postErr == nil {
			postErr = insertMessagePostReceipt(ctx, tx, &message, req.RequestID, requestHash(req), by)
		}
		if postErr == nil {
			// A delivery-only obligation, so the broker's wake job reaches the
			// lead (the relay leaves typed notices to the broker).
			postErr = s.createObligations(ctx, tx, message, req, false)
		}
		if postErr != nil {
			if _, err = tx.ExecContext(ctx, `ROLLBACK TO monitor_message`); err != nil {
				return notice, "unknown", err
			}
			deliveryErr = postErr
		} else {
			notice.LastNotifiedAt, notice.MessageSeq = now, message.Seq
			notice.NotificationCount++
			notice.FailureCount, notice.LastError, notice.PendingRequestID = 0, "", ""
		}
		if _, err = tx.ExecContext(ctx, `RELEASE monitor_message`); err != nil {
			return notice, "unknown", err
		}
	}
	if deliveryErr != nil {
		notice.FailureCount++
		notice.LastError = deliveryErr.Error()
		if len(notice.LastError) > api.MaxTextLen {
			notice.LastError = "schedule monitor delivery failed"
		}
		return finish("delivery_failed", deliveryErr)
	}
	return finish("notified", nil)
}

// ScheduleMonitorBackoff doubles without overflowing time.Duration.
func ScheduleMonitorBackoff(initial, maximum time.Duration, count int) time.Duration {
	d := initial
	for i := 1; i < count && d < maximum; i++ {
		if d > maximum/2 {
			return maximum
		}
		d *= 2
	}
	return d
}
