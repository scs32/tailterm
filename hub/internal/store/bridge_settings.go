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

// Discord bridge settings (wi_dc04ac6a9ccf7402): the owner's non-secret
// bridge configuration, one row. No token is ever stored here.
const bridgeSettingsSchema = `
CREATE TABLE IF NOT EXISTS bridge_settings (
	id INTEGER PRIMARY KEY CHECK(id=1),
	settings_json TEXT NOT NULL,
	revision INTEGER NOT NULL,
	updated_at TEXT NOT NULL,
	by_node TEXT NOT NULL,
	by_user TEXT NOT NULL
)`

func migrateBridgeSettings(db *sql.DB) error {
	_, err := db.Exec(bridgeSettingsSchema)
	return err
}

// BridgeSettings returns the saved settings; Settings is nil and Revision 0
// before the first save.
func (s *Store) BridgeSettings(ctx context.Context) (api.BridgeSettingsRecord, error) {
	return bridgeSettings(ctx, s.db)
}

func bridgeSettings(ctx context.Context, q queryRower) (api.BridgeSettingsRecord, error) {
	var rec api.BridgeSettingsRecord
	var raw, updated, node, user string
	err := q.QueryRowContext(ctx, `SELECT settings_json,revision,updated_at,by_node,by_user FROM bridge_settings WHERE id=1`).Scan(&raw, &rec.Revision, &updated, &node, &user)
	if errors.Is(err, sql.ErrNoRows) {
		return api.BridgeSettingsRecord{}, nil
	}
	if err != nil {
		return rec, err
	}
	var settings api.BridgeSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return rec, fmt.Errorf("bridge settings: %w", err)
	}
	at, _ := time.Parse(time.RFC3339Nano, updated)
	rec.Settings, rec.UpdatedAt, rec.UpdatedBy = &settings, &at, &api.Caller{Node: node, User: user}
	return rec, nil
}

// PutBridgeSettings saves the settings when req.ExpectedRevision is still the
// saved revision (api.ErrConflict otherwise) and the values are valid
// (api.ErrInvalid), including that helperTask names an existing project.
func (s *Store) PutBridgeSettings(ctx context.Context, req api.PutBridgeSettingsRequest, by api.Caller) (api.BridgeSettingsRecord, error) {
	settings, err := api.ValidateBridgeSettings(req.Settings)
	if err != nil {
		return api.BridgeSettingsRecord{}, err
	}
	if req.ExpectedRevision < 0 {
		return api.BridgeSettingsRecord{}, fmt.Errorf("%w: expectedRevision must not be negative", api.ErrInvalid)
	}
	if settings.HelperTask != "" {
		if _, err := s.GetTask(ctx, settings.HelperTask); errors.Is(err, api.ErrNotFound) {
			return api.BridgeSettingsRecord{}, fmt.Errorf("%w: helperTask names no project", api.ErrInvalid)
		} else if err != nil {
			return api.BridgeSettingsRecord{}, err
		}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return api.BridgeSettingsRecord{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.BridgeSettingsRecord{}, err
	}
	defer tx.Rollback()
	current, err := bridgeSettings(ctx, tx)
	if err != nil {
		return api.BridgeSettingsRecord{}, err
	}
	if current.Revision != req.ExpectedRevision {
		return api.BridgeSettingsRecord{}, fmt.Errorf("%w: the bridge settings changed elsewhere (saved revision %d, expected %d); reload and try again", api.ErrConflict, current.Revision, req.ExpectedRevision)
	}
	now := s.now()
	next := current.Revision + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO bridge_settings (id,settings_json,revision,updated_at,by_node,by_user) VALUES (1,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET settings_json=excluded.settings_json, revision=excluded.revision, updated_at=excluded.updated_at, by_node=excluded.by_node, by_user=excluded.by_user`,
		string(raw), next, now.Format(time.RFC3339Nano), by.Node, by.User); err != nil {
		return api.BridgeSettingsRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.BridgeSettingsRecord{}, err
	}
	return api.BridgeSettingsRecord{Settings: &settings, Revision: next, UpdatedAt: &now, UpdatedBy: &by}, nil
}
