package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var profileFixture = json.RawMessage(`{"format":"tailterm-profile","version":1,"iv":"AAAAAAAAAAAAAAAA","ciphertext":"AAAAAAAAAAAAAAAAAAAAAA=="}`)
var profileFixtureV2 = json.RawMessage(`{"format":"tailterm-profile","version":2,"iv":"AAAAAAAAAAAAAAAA","ciphertext":"AAAAAAAAAAAAAAAAAAAAAA=="}`)

func TestProfilesIsolationCASAndHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := context.Background()
	id, _ := s.ProfileServiceID(ctx)
	alice, bob := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, name := range []string{"../x", "Alice", "", "a b"} {
		if _, err := s.CreateProfile(ctx, name, alice, profileFixture); err == nil {
			t.Fatal("accepted name", name)
		}
	}
	p, err := s.CreateProfile(ctx, "alice", alice, profileFixture)
	if err != nil || p.Revision != 1 {
		t.Fatal(p, err)
	}
	if _, err := s.CreateProfile(ctx, "alice", bob, profileFixture); !errors.Is(err, ErrProfileConflict) {
		t.Fatal("overwrote existing", err)
	}
	if _, err := s.CreateProfile(ctx, "bob", bob, profileFixture); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alice", "missing"} {
		if _, err := s.ReadProfile(ctx, name, bob); !errors.Is(err, ErrProfileAuth) {
			t.Fatal("leaked profile", err)
		}
	}
	if _, err := s.WriteProfile(ctx, "alice", bob, 1, profileFixture); !errors.Is(err, ErrProfileAuth) {
		t.Fatal("unauthorized update", err)
	}
	for rev := int64(1); rev <= 12; rev++ {
		if _, err := s.WriteProfile(ctx, "alice", alice, rev, profileFixture); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.WriteProfile(ctx, "alice", alice, 1, profileFixture); !errors.Is(err, ErrProfileConflict) {
		t.Fatal("lost update", err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM profile_history WHERE username='alice'`).Scan(&count)
	if count != 10 {
		t.Fatal("history", count)
	}
	var hash string
	s.db.QueryRow(`SELECT key_hash FROM profiles WHERE username='alice'`).Scan(&hash)
	if hash == alice {
		t.Fatal("stored raw credential")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	nextID, _ := s.ProfileServiceID(ctx)
	if id != nextID {
		t.Fatal("changed identity")
	}
	p, err = s.ReadProfile(ctx, "alice", alice)
	if err != nil || p.Revision != 13 {
		t.Fatal("restart", p, err)
	}
}

func TestProfileEnvelopeDowngradeIsAtomic(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	key := strings.Repeat("a", 64)
	profile, err := s.CreateProfile(ctx, "alice", key, profileFixtureV2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteProfile(ctx, "alice", key, profile.Revision, profileFixture); !errors.Is(err, ErrProfileDowngrade) {
		t.Fatalf("downgrade = %v", err)
	}
	current, err := s.ReadProfile(ctx, "alice", key)
	if err != nil || current.Revision != profile.Revision || string(current.Envelope) != string(profileFixtureV2) {
		t.Fatalf("mutated current: %#v %v", current, err)
	}
	var history int
	if err := s.db.QueryRow(`SELECT count(*) FROM profile_history WHERE username='alice'`).Scan(&history); err != nil || history != 0 {
		t.Fatalf("history = %d, %v", history, err)
	}
	if _, err := s.WriteProfile(ctx, "alice", key, profile.Revision+1, profileFixture); !errors.Is(err, ErrProfileConflict) {
		t.Fatalf("CAS did not precede downgrade: %v", err)
	}
}
