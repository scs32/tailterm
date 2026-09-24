// Package bridge mirrors Tailterm projects to Discord and carries the owner's
// Discord input back to the hub (broker phase 2b, docs/broker-phase-2b.md).
//
// The hub stays authoritative. The bridge talks to it only over HTTP with
// its own route-limited credential and keeps its own durable state here:
// project↔channel mappings and cursors, an outbox of sends that survive
// restarts, and receipts that make Discord redeliveries harmless.
package bridge

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS channels (
  task_id TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL DEFAULT '',
  archived INTEGER NOT NULL DEFAULT 0,
  mirror_after INTEGER NOT NULL DEFAULT 0,
  card_message_id TEXT NOT NULL DEFAULT '',
  card_hash TEXT NOT NULL DEFAULT '',
  card_edited_at TEXT NOT NULL DEFAULT '',
  ingest_after TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS channels_channel ON channels(channel_id) WHERE channel_id<>'';
CREATE TABLE IF NOT EXISTS outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key TEXT NOT NULL UNIQUE,
  task_id TEXT NOT NULL,
  seq INTEGER NOT NULL DEFAULT 0,
  agent_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  payload TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL,
  discord_id TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS outbox_pending ON outbox(state, id);
CREATE TABLE IF NOT EXISTS mirrored (
  discord_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  agent_id TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS inbound (
  source_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  task_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  result TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);`

// Outbox and inbound states.
const (
	statePending = "pending"
	stateSent    = "sent"
	stateFailed  = "failed"
	stateDone    = "done"
)

// State is the bridge's own SQLite database. One bridge process owns it.
type State struct {
	db  *sql.DB
	now func() time.Time
}

func OpenState(path string) (*State, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &State{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *State) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

// Mapping is one project's channel and cursors.
type Mapping struct {
	TaskID        string
	ChannelID     string
	Archived      bool
	MirrorAfter   int64
	CardMessageID string
	CardHash      string
	CardEditedAt  time.Time
	IngestAfter   string
}

const mappingCols = `task_id,channel_id,archived,mirror_after,card_message_id,card_hash,card_edited_at,ingest_after`

func scanMapping(row interface{ Scan(...any) error }) (Mapping, error) {
	var m Mapping
	var edited string
	err := row.Scan(&m.TaskID, &m.ChannelID, &m.Archived, &m.MirrorAfter, &m.CardMessageID, &m.CardHash, &edited, &m.IngestAfter)
	m.CardEditedAt = parseTS(edited)
	return m, err
}

func (s *State) Mappings(ctx context.Context) ([]Mapping, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mappingCols+` FROM channels ORDER BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mapping
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ErrNoMapping means the task or channel has no mapping.
var ErrNoMapping = errors.New("no channel mapping")

func (s *State) Mapping(ctx context.Context, taskID string) (Mapping, error) {
	m, err := scanMapping(s.db.QueryRowContext(ctx, `SELECT `+mappingCols+` FROM channels WHERE task_id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNoMapping
	}
	return m, err
}

func (s *State) MappingByChannel(ctx context.Context, channelID string) (Mapping, error) {
	if channelID == "" {
		return Mapping{}, ErrNoMapping
	}
	m, err := scanMapping(s.db.QueryRowContext(ctx, `SELECT `+mappingCols+` FROM channels WHERE channel_id=?`, channelID))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNoMapping
	}
	return m, err
}

// SetChannel records a project's channel, starting its mirror after the
// given hub sequence. An existing mapping keeps its cursors.
func (s *State) SetChannel(ctx context.Context, taskID, channelID string, mirrorAfter int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO channels (task_id,channel_id,mirror_after) VALUES (?,?,?)
ON CONFLICT(task_id) DO UPDATE SET channel_id=excluded.channel_id`, taskID, channelID, mirrorAfter)
	return err
}

// ForgetChannel clears a channel Discord no longer has, so the project is
// provisioned again; the mirror cursor and pending sends are kept.
func (s *State) ForgetChannel(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET channel_id='',card_message_id='',card_hash='' WHERE task_id=?`, taskID)
	return err
}

func (s *State) SetArchived(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET archived=1 WHERE task_id=?`, taskID)
	return err
}

func (s *State) SetCard(ctx context.Context, taskID, messageID, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET card_message_id=?,card_hash=?,card_edited_at=? WHERE task_id=?`, messageID, hash, ts(s.now()), taskID)
	return err
}

func (s *State) SetIngestAfter(ctx context.Context, taskID, messageID string) error {
	// Discord IDs are snowflakes: compare as numbers (length, then text).
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET ingest_after=? WHERE task_id=? AND (length(ingest_after)<length(?) OR (length(ingest_after)=length(?) AND ingest_after<?))`,
		messageID, taskID, messageID, messageID, messageID)
	return err
}

// OutboxRow is one intended Discord send, persisted before any network call.
type OutboxRow struct {
	ID        int64
	Key       string
	TaskID    string
	Seq       int64
	AgentID   string
	Kind      string
	Payload   string
	State     string
	Attempts  int
	NextAt    time.Time
	DiscordID string
	LastError string
}

const outboxCols = `id,key,task_id,seq,agent_id,kind,payload,state,attempts,next_at,discord_id,last_error`

func scanOutbox(row interface{ Scan(...any) error }) (OutboxRow, error) {
	var o OutboxRow
	var next string
	err := row.Scan(&o.ID, &o.Key, &o.TaskID, &o.Seq, &o.AgentID, &o.Kind, &o.Payload, &o.State, &o.Attempts, &next, &o.DiscordID, &o.LastError)
	o.NextAt = parseTS(next)
	return o, err
}

// Enqueue adds sends and advances the project's mirror cursor in one
// transaction, so a crash either records both or neither. Keys are unique:
// re-enqueueing the same key is a no-op.
func (s *State) Enqueue(ctx context.Context, taskID string, mirrorAfter int64, rows []OutboxRow) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := ts(s.now())
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox (key,task_id,seq,agent_id,kind,payload,next_at,created_at) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(key) DO NOTHING`,
			r.Key, r.TaskID, r.Seq, r.AgentID, r.Kind, r.Payload, now, now); err != nil {
			return err
		}
	}
	if mirrorAfter > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET mirror_after=? WHERE task_id=? AND mirror_after<?`, mirrorAfter, taskID, mirrorAfter); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingOutbox lists sends that are due, in creation order.
func (s *State) PendingOutbox(ctx context.Context, limit int) ([]OutboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboxCols+` FROM outbox WHERE state=? ORDER BY id LIMIT ?`, statePending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		o, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkSent records Discord's acceptance and, for mirrored board messages,
// which hub message and agent the Discord message stands for.
func (s *State) MarkSent(ctx context.Context, o OutboxRow, discordID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state=?,discord_id=?,last_error='' WHERE id=?`, stateSent, discordID, o.ID); err != nil {
		return err
	}
	if o.Seq > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mirrored (discord_id,task_id,seq,agent_id) VALUES (?,?,?,?) ON CONFLICT(discord_id) DO NOTHING`, discordID, o.TaskID, o.Seq, o.AgentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkRetry schedules another attempt; the same nonce and marker are reused.
func (s *State) MarkRetry(ctx context.Context, id int64, next time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts=attempts+1,next_at=?,last_error=? WHERE id=?`, ts(next), reason, id)
	return err
}

func (s *State) MarkFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET state=?,last_error=? WHERE id=?`, stateFailed, reason, id)
	return err
}

// Mirrored is the hub message a Discord message stands for.
type Mirrored struct {
	TaskID  string
	Seq     int64
	AgentID string
}

func (s *State) MirroredMessage(ctx context.Context, discordID string) (Mirrored, bool, error) {
	var m Mirrored
	err := s.db.QueryRowContext(ctx, `SELECT task_id,seq,agent_id FROM mirrored WHERE discord_id=?`, discordID).Scan(&m.TaskID, &m.Seq, &m.AgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// Inbound is one Discord message or interaction the bridge has taken on.
type Inbound struct {
	SourceID string
	Kind     string
	TaskID   string
	State    string
	Result   string
	Payload  string
	Attempts int
}

// ClaimInbound records a Discord message or interaction the first time it
// is seen. It returns false for a redelivery, which must do nothing.
func (s *State) ClaimInbound(ctx context.Context, sourceID, kind, taskID, payload string) (bool, error) {
	now := ts(s.now())
	res, err := s.db.ExecContext(ctx, `INSERT INTO inbound (source_id,kind,task_id,state,payload,next_at,created_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(source_id) DO NOTHING`,
		sourceID, kind, taskID, statePending, payload, now, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *State) FinishInbound(ctx context.Context, sourceID, result string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbound SET state=?,result=? WHERE source_id=?`, stateDone, result, sourceID)
	return err
}

func (s *State) RetryInbound(ctx context.Context, sourceID string, next time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbound SET attempts=attempts+1,next_at=?,result=? WHERE source_id=?`, ts(next), reason, sourceID)
	return err
}

// PendingInbound lists owner messages whose hub post has not succeeded yet.
func (s *State) PendingInbound(ctx context.Context, now time.Time) ([]Inbound, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_id,kind,task_id,state,result,payload,attempts FROM inbound WHERE state=? AND kind='message' AND next_at<=? ORDER BY created_at`, statePending, ts(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Inbound
	for rows.Next() {
		var in Inbound
		if err := rows.Scan(&in.SourceID, &in.Kind, &in.TaskID, &in.State, &in.Result, &in.Payload, &in.Attempts); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
