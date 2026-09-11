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
	LastNotifiedAt    time.Time
	NotificationCount int
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
	return err
}

func (s *Store) GetScheduleMonitorNotice(ctx context.Context, taskID, fingerprint string) (ScheduleMonitorNotice, error) {
	if !api.ValidID(taskID, "tsk") || fingerprint == "" {
		return ScheduleMonitorNotice{}, api.ErrInvalid
	}
	var notice ScheduleMonitorNotice
	var first, observed, notified string
	err := s.db.QueryRowContext(ctx, `SELECT task_id,fingerprint,first_detected_at,last_observed_at,last_notified_at,notification_count,message_seq,last_error
FROM schedule_monitor_notices WHERE task_id=? AND fingerprint=?`, taskID, fingerprint).Scan(
		&notice.TaskID, &notice.Fingerprint, &first, &observed, &notified, &notice.NotificationCount, &notice.MessageSeq, &notice.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return notice, api.ErrNotFound
	}
	notice.FirstDetectedAt, notice.LastObservedAt = parseTS(first), parseTS(observed)
	if notified != "" {
		notice.LastNotifiedAt = parseTS(notified)
	}
	return notice, err
}

// SaveScheduleMonitorNotice records observation and successful/failed delivery.
// It intentionally does not create events, alter Queue entries, or touch agents.
func (s *Store) SaveScheduleMonitorNotice(ctx context.Context, notice ScheduleMonitorNotice) error {
	if !api.ValidID(notice.TaskID, "tsk") || notice.Fingerprint == "" || notice.FirstDetectedAt.IsZero() || notice.LastObservedAt.IsZero() || notice.NotificationCount < 0 || notice.MessageSeq < 0 {
		return api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_monitor_notices
(task_id,fingerprint,first_detected_at,last_observed_at,last_notified_at,notification_count,message_seq,last_error)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(task_id,fingerprint) DO UPDATE SET
last_observed_at=excluded.last_observed_at,last_notified_at=excluded.last_notified_at,
notification_count=excluded.notification_count,message_seq=excluded.message_seq,last_error=excluded.last_error`,
		notice.TaskID, notice.Fingerprint, ts(notice.FirstDetectedAt), ts(notice.LastObservedAt),
		func() string {
			if notice.LastNotifiedAt.IsZero() {
				return ""
			}
			return ts(notice.LastNotifiedAt)
		}(),
		notice.NotificationCount, notice.MessageSeq, notice.LastError)
	return err
}
