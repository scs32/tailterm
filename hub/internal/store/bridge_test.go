package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// A repeated nudge request ID returns the original nudge, even after the
// cooldown, so a resumed interaction never wakes the agent twice.
func TestNudgeRequestIDIsIdempotent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	by := api.Caller{Node: "workspace", User: "owner"}
	task, _ := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Nudges"}, by)
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "builder", Host: "h", Session: "s"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, Text: "status please"}, by); err != nil {
		t.Fatal(err)
	}
	obligations, _ := s.ListObligations(ctx, task.ID, ObligationFilter{}, time.Now())
	now := time.Now().UTC()
	s.SetClockForTest(func() time.Time { return now })
	first, err := s.NudgeObligation(ctx, task.ID, obligations[0].ID, "discord-interaction-1", by)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute) // past the cooldown, as a crash recovery would be
	again, err := s.NudgeObligation(ctx, task.ID, obligations[0].ID, "discord-interaction-1", by)
	if err != nil || again.WakeJobID != first.WakeJobID {
		t.Fatalf("replayed nudge = %+v %v, want the original %s", again, err, first.WakeJobID)
	}
	var nudges, jobs int
	_ = s.db.QueryRow(`SELECT count(*) FROM obligation_nudges`).Scan(&nudges)
	_ = s.db.QueryRow(`SELECT count(*) FROM wake_jobs WHERE id=?`, first.WakeJobID).Scan(&jobs)
	if nudges != 1 || jobs != 1 {
		t.Fatalf("%d nudges recorded, want 1", nudges)
	}
	if other, err := s.NudgeObligation(ctx, task.ID, obligations[0].ID, "discord-interaction-2", by); err != nil || other.WakeJobID == first.WakeJobID {
		t.Fatalf("a new request ID after the cooldown = %+v %v", other, err)
	}
	if _, err := s.NudgeObligation(ctx, task.ID, obligations[0].ID, "bad id with spaces", by); err == nil {
		t.Error("an invalid request ID was accepted")
	}
}

// A database created before request IDs existed gains the column.
func TestNudgeTableMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP INDEX obligation_nudges_request; ALTER TABLE obligation_nudges DROP COLUMN request_id`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopening an older database: %v", err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('obligation_nudges') WHERE name='request_id'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("request_id column after migration: %d %v", n, err)
	}
}
