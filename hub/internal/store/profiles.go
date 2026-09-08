package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const MaxProfileBytes = 2 * 1024 * 1024

var ProfileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var ProfileKey = regexp.MustCompile(`^[0-9a-f]{64}$`)
var ErrProfileAuth = errors.New("profile authentication failed")
var ErrProfileConflict = errors.New("profile changed on another device")

type Profile struct {
	Username  string          `json:"username"`
	Revision  int64           `json:"revision"`
	UpdatedAt string          `json:"updatedAt"`
	Envelope  json.RawMessage `json:"envelope"`
}

func validEnvelope(raw json.RawMessage) bool {
	if len(raw) > MaxProfileBytes {
		return false
	}
	var e struct {
		Format     string `json:"format"`
		Version    int    `json:"version"`
		IV         string `json:"iv"`
		Ciphertext string `json:"ciphertext"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Format != "tailterm-profile" || e.Version != 1 {
		return false
	}
	return validProfileBase64(e.IV, 12) && validProfileBase64(e.Ciphertext, 0)
}
func profileHash(key string) string { h := sha256.Sum256([]byte(key)); return hex.EncodeToString(h[:]) }
func (s *Store) ProfileServiceID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM profile_meta WHERE key='instance'`).Scan(&id)
	return id, err
}
func (s *Store) ReadProfile(ctx context.Context, username, key string) (Profile, error) {
	var p Profile
	var expected string
	if !ProfileName.MatchString(username) || !ProfileKey.MatchString(key) {
		return p, ErrProfileAuth
	}
	err := s.db.QueryRowContext(ctx, `SELECT username,revision,updated_at,envelope,key_hash FROM profiles WHERE username=?`, username).Scan(&p.Username, &p.Revision, &p.UpdatedAt, &p.Envelope, &expected)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrProfileAuth
	}
	if err != nil {
		return p, err
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(profileHash(key))) != 1 {
		return Profile{}, ErrProfileAuth
	}
	return p, nil
}
func (s *Store) CreateProfile(ctx context.Context, username, key string, envelope json.RawMessage) (Profile, error) {
	if !ProfileName.MatchString(username) || !ProfileKey.MatchString(key) || !validEnvelope(envelope) {
		return Profile{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM profiles WHERE username=?`, username).Scan(&count); err != nil {
		return Profile{}, err
	}
	if count != 0 {
		return Profile{}, ErrProfileConflict
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM profiles`).Scan(&count); err != nil {
		return Profile{}, err
	}
	if count >= 32 {
		return Profile{}, api.ErrLimit
	}
	now := s.now().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO profiles(username,key_hash,revision,updated_at,envelope) VALUES(?,?,1,?,?)`, username, profileHash(key), now, []byte(envelope))
	return Profile{Username: username, Revision: 1, UpdatedAt: now, Envelope: envelope}, err
}
func (s *Store) WriteProfile(ctx context.Context, username, key string, revision int64, envelope json.RawMessage) (Profile, error) {
	if revision < 1 || !validEnvelope(envelope) {
		return Profile{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	previous, err := s.ReadProfile(ctx, username, key)
	if err != nil {
		return Profile{}, err
	}
	if previous.Revision != revision {
		return Profile{}, ErrProfileConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Profile{}, err
	}
	defer tx.Rollback()
	// Keep recent encrypted versions so conflict resolution is recoverable.
	if _, err = tx.ExecContext(ctx, `INSERT INTO profile_history(username,revision,updated_at,envelope) VALUES(?,?,?,?)`, username, previous.Revision, previous.UpdatedAt, []byte(previous.Envelope)); err != nil {
		return Profile{}, err
	}
	now := s.now().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE profiles SET revision=revision+1,updated_at=?,envelope=? WHERE username=?`, now, []byte(envelope), username); err != nil {
		return Profile{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM profile_history WHERE username=? AND revision <= ?`, username, revision-10); err != nil {
		return Profile{}, err
	}
	if err = tx.Commit(); err != nil {
		return Profile{}, err
	}
	return Profile{Username: username, Revision: revision + 1, UpdatedAt: now, Envelope: envelope}, nil
}

func validProfileBase64(s string, length int) bool {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	return err == nil && ((length > 0 && len(b) == length) || (length == 0 && len(b) >= 16))
}
