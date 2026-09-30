package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Backlog steward tests (wi_5b4b94dbc9a11e8b, order #14942) use an isolated
// SQLite hub per test and synthetic agents only.

var stewardBy = api.Caller{Node: "fixture", User: "owner"}

func stewardStore(t *testing.T) (*Store, api.Task) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	task, err := s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Steward fixture"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	return s, task
}

func stewardRequest(id, name string) api.AddAgentRequest {
	return api.AddAgentRequest{Role: api.AgentRoleBacklogSteward, AgentID: id, Name: name, Host: "mini", Session: "tt-steward-" + strings.TrimPrefix(id, "agt_"), Runtime: "claude", Cwd: "/tmp"}
}

func addSteward(t *testing.T, s *Store, task, name string) api.Agent {
	t.Helper()
	a, err := s.AddAgent(context.Background(), task, stewardRequest(api.NewID("agt"), name), stewardBy)
	if err != nil {
		t.Fatalf("add steward %s: %v", name, err)
	}
	return a
}

func setAgentStatus(t *testing.T, s *Store, id, status string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, status, id); err != nil {
		t.Fatal(err)
	}
}

func wantStewardRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *api.StewardRefusal
	if !errors.As(err, &refusal) || refusal.Code != code || !errors.Is(err, api.ErrConflict) {
		t.Fatalf("want steward refusal %s, got %v", code, err)
	}
}

func TestStewardSpawnRegistersPersistentRole(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	a := addSteward(t, s, task.ID, "backlog-steward")
	if a.Role != api.AgentRoleBacklogSteward || a.ParentAgentID != "" || a.WorkItem != nil || a.Status != api.AgentStarting {
		t.Fatalf("steward registration: %+v", a)
	}
	// Persistent-role admission rules: a stable ID, no parent, no item.
	bad := stewardRequest("", "steward-no-id")
	if _, err := s.AddAgent(ctx, task.ID, bad, stewardBy); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("steward without a stable ID: %v", err)
	}
	bad = stewardRequest(api.NewID("agt"), "steward-parented")
	bad.ParentAgentID = a.ID
	if _, err := s.AddAgent(ctx, task.ID, bad, stewardBy); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("parented steward: %v", err)
	}
	// Replaying the exact registration is idempotent.
	again, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Role: a.Role, AgentID: a.ID, Name: a.Name, Host: a.Host, Session: a.Session, Runtime: a.Runtime, Cwd: a.Cwd}, stewardBy)
	if err != nil || again.ID != a.ID || again.RunID != a.RunID {
		t.Fatalf("replayed registration: %+v %v", again, err)
	}
	// A template digest is recorded for the steward's exact run.
	digest := strings.Repeat("ab", 32)
	b := stewardRequest(api.NewID("agt"), "other-project-steward")
	b.TemplateDigest = digest
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := s.AddAgent(ctx, other.ID, b, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := s.db.QueryRow(`SELECT template_digest FROM steward_runs WHERE run_id=? AND agent_id=?`, ob.RunID, ob.ID).Scan(&saved); err != nil || saved != digest {
		t.Fatalf("steward run digest: %q %v", saved, err)
	}
}

func TestStewardOnePerProject(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	first := addSteward(t, s, task.ID, "backlog-steward")
	second := stewardRequest(api.NewID("agt"), "backlog-steward-2")
	for _, status := range []string{api.AgentStarting, api.AgentRunning, api.AgentDone, api.AgentRetired, api.AgentExited} {
		setAgentStatus(t, s, first.ID, status)
		_, err := s.AddAgent(ctx, task.ID, second, stewardBy)
		wantStewardRefusal(t, err, api.StewardRefusedActive)
	}
	// The exited steward restarts under its own identity by expected run; it
	// is the same slot holder, and a second identity is still refused.
	restarted, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Role: first.Role, AgentID: first.ID, ExpectedRunID: first.RunID, Name: first.Name, Host: first.Host,
		Session: first.Session, Runtime: first.Runtime, Cwd: first.Cwd}, stewardBy)
	if err != nil || restarted.RunID == first.RunID || restarted.Status != api.AgentStarting {
		t.Fatalf("exited steward restart: %+v %v", restarted, err)
	}
	_, err = s.AddAgent(ctx, task.ID, second, stewardBy)
	wantStewardRefusal(t, err, api.StewardRefusedActive)
	// A restart by name under a new identity is refused.
	setAgentStatus(t, s, first.ID, api.AgentExited)
	byName := stewardRequest(api.NewID("agt"), first.Name)
	if _, err := s.AddAgent(ctx, task.ID, byName, stewardBy); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("restart by name without the stable ID: %v", err)
	}
	// Another project is unaffected.
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	addSteward(t, s, other.ID, "backlog-steward")
	// A close frees the slot.
	setAgentStatus(t, s, first.ID, api.AgentClosed)
	if a, err := s.AddAgent(ctx, task.ID, second, stewardBy); err != nil || a.ID != second.AgentID {
		t.Fatalf("steward after close: %+v %v", a, err)
	}
	// The index backs the rule: a raw second holder row is a constraint error,
	// which admission maps to the named refusal.
	_, rawErr := s.db.Exec(`INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		api.NewID("agt"), task.ID, "raw", "mini", "raw", "claude", "/tmp", "", api.AgentRoleBacklogSteward, api.AgentRunning, "", ts(s.now()), ts(s.now()), api.NewID("run"), "", "", "", false, "")
	if rawErr == nil {
		t.Fatal("the one-steward index accepted a second holder")
	}
	wantStewardRefusal(t, stewardUniqueViolation(ctx, s.db, task.ID, rawErr), api.StewardRefusedActive)
}

func TestStewardOnePerProjectConcurrentAdmission(t *testing.T) {
	s, task := stewardStore(t)
	ctx := context.Background()
	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AddAgent(ctx, task.ID, stewardRequest(api.NewID("agt"), "steward-"+string(rune('a'+i))), stewardBy)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
			continue
		}
		wantStewardRefusal(t, err, api.StewardRefusedActive)
	}
	var holders int
	if err := s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status<>'closed'`, task.ID, api.AgentRoleBacklogSteward).Scan(&holders); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || holders != 1 {
		t.Fatalf("concurrent admissions: %d succeeded, %d stewards", ok, holders)
	}
}
