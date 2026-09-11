package store

import (
	"context"
	"database/sql"
	"errors"
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
	return nil
}

func (s *Store) GetScheduleMonitorNotice(ctx context.Context, taskID, fingerprint string) (ScheduleMonitorNotice, error) {
	if !api.ValidID(taskID, "tsk") || fingerprint == "" {
		return ScheduleMonitorNotice{}, api.ErrInvalid
	}
	var notice ScheduleMonitorNotice
	var first, observed, attempted, notified string
	err := s.db.QueryRowContext(ctx, `SELECT task_id,fingerprint,first_detected_at,last_observed_at,last_attempt_at,last_notified_at,notification_count,failure_count,message_seq,last_error
FROM schedule_monitor_notices WHERE task_id=? AND fingerprint=?`, taskID, fingerprint).Scan(
		&notice.TaskID, &notice.Fingerprint, &first, &observed, &attempted, &notified, &notice.NotificationCount, &notice.FailureCount, &notice.MessageSeq, &notice.LastError)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_monitor_notices
(task_id,fingerprint,first_detected_at,last_observed_at,last_attempt_at,last_notified_at,notification_count,failure_count,message_seq,last_error)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(task_id,fingerprint) DO UPDATE SET
last_observed_at=excluded.last_observed_at,last_attempt_at=excluded.last_attempt_at,last_notified_at=excluded.last_notified_at,
notification_count=excluded.notification_count,failure_count=excluded.failure_count,message_seq=excluded.message_seq,last_error=excluded.last_error`,
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
		notice.NotificationCount, notice.FailureCount, notice.MessageSeq, notice.LastError)
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
