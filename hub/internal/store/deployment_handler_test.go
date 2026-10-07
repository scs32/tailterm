package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// f5: the deployer's handler requests reach the project database handler by
// the project rule, even in parallel mode after its entry finished.
func TestReleaseHandlerResolvesProjectHandlerForDeployer(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	if _, err := s.db.Exec(`INSERT INTO team_queue_settings(task_id,concurrency_limit) VALUES(?,3)`, task.ID); err != nil {
		t.Fatal(err)
	}
	var itemID string
	var itemRevision int64
	if err := s.db.QueryRow(`SELECT item_id,item_revision FROM team_queue_entries WHERE id=?`, entry).Scan(&itemID, &itemRevision); err != nil {
		t.Fatal(err)
	}
	add := func(name, role string, created time.Time) api.Agent {
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "fixture", Session: name, Role: role}, by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`UPDATE agents SET created_at=? WHERE id=?`, ts(created), a.ID); err != nil {
			t.Fatal(err)
		}
		return a
	}
	base := time.Now().Add(time.Hour)
	newer := add("handler-new", api.AgentRoleDatabaseHandler, base)
	member := add("builder", "", base.Add(time.Minute))
	resolve := func() (api.Agent, error) { return s.ReleaseHandler(ctx, task.ID, d.ID, d.RunID) }
	if got, err := resolve(); err != nil || got.ID != newer.ID {
		t.Fatalf("newest open handler: %+v %v", got, err)
	}
	// The deployer posts its typed request to the resolved handler, with and
	// without an item link to the finished entry, in parallel mode.
	for i, links := range [][]api.MessageWorkItem{nil, {{ItemTaskID: task.ID, ItemID: itemID, ItemRevision: itemRevision, Relationship: "primary"}}} {
		got, _ := resolve()
		m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: d.ID, RunID: d.RunID, To: got.ID, WorkItems: links, RequestID: "deployer-request-" + string(rune('a'+i)), Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: got.Name, Subject: "Import immutable inputs for this release job", Body: api.EnvelopeBody{Ask: "prepare inputs"}}}, by)
		if err != nil || m.To != newer.ID {
			t.Fatalf("deployer request %d: %+v %v", i, m, err)
		}
	}
	// Item-team role posting is unchanged: no item link is still refused.
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: member.ID, RunID: member.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: "role:database_handler", Subject: "Record the plan revision please", Body: api.EnvelopeBody{Ask: "please"}}}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("member role post in parallel mode: %v", err)
	}
	// A retired handler is excluded; the older open one takes over.
	if _, err := s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, newer.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := resolve(); err != nil || got.ID != h.ID {
		t.Fatalf("retired excluded: %+v %v", got, err)
	}
	// An explicit primary wins over a newer open handler; a retired primary does not.
	if _, err := s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentRunning, newer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET primary_handler_id=? WHERE id=?`, h.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := resolve(); err != nil || got.ID != h.ID {
		t.Fatalf("explicit primary: %+v %v", got, err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, h.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := resolve(); err != nil || got.ID != newer.ID {
		t.Fatalf("retired primary: %+v %v", got, err)
	}
	// The prepared successor of an open rotation does not hold the role yet.
	now := ts(time.Now())
	if _, err := s.db.Exec(`INSERT INTO handler_rotations(id,task_id,request_id,payload_hash,state,reason,trigger_kind,handler_revision,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,created_at,updated_at) VALUES('hrot_fixture',?,'rotate','hash','prepared','fixture','owner',1,?,?,?,?,?,?,?)`, task.ID, h.ID, h.RunID, h.Name, newer.ID, newer.Name, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("prepared successor or retired-only project must conflict clearly: %v", err)
	}
	// Only the exact deployment run may ask.
	if _, err := s.ReleaseHandler(ctx, task.ID, member.ID, member.RunID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("non-deployer resolved the handler: %v", err)
	}
}

// f2: a directed request to the deployer never blocks its release actions or
// posts, while any other agent holding the same overdue work is still gated.
func TestAckGateExemptsDeploymentAgent(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	start := time.Now().UTC()
	s.now = func() time.Time { return start }
	direct := func(to api.Agent, key string) {
		if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: h.ID, RunID: h.RunID, To: to.ID, RequestID: key, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: to.Name, Subject: "Please report the release journal state", Body: api.EnvelopeBody{Ask: "report"}}}, by); err != nil {
			t.Fatal(err)
		}
	}
	direct(d, "to-deployer")
	s.now = func() time.Time { return start.Add(api.ObligationAckGrace + time.Minute) }
	// Claim freshness reads the store clock: the deployer heartbeats at it.
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(s.now()), d.ID); err != nil {
		t.Fatal(err)
	}
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "check", "merged", "check", "block"} {
		if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: op + "-" + j.State, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}); err != nil {
			t.Fatalf("%s gated: %v", op, err)
		}
	}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: d.ID, RunID: d.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Release failed and requires recovery", Body: api.EnvelopeBody{Text: "escalation"}}}, by); err != nil {
		t.Fatalf("deployer escalation gated: %v", err)
	}
	// The same overdue request still gates an ordinary agent.
	other, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "fixture", Session: "builder"}, by)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return start }
	direct(other, "to-builder")
	s.now = func() time.Time { return start.Add(api.ObligationAckGrace + time.Minute) }
	var unacked *api.UnacknowledgedError
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: other.ID, RunID: other.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Status of the release journal work", Body: api.EnvelopeBody{Text: "busy"}}}, by); !errors.As(err, &unacked) {
		t.Fatalf("ordinary agent not gated: %v", err)
	}
}

// f2: finish and refuse, the other fenced release actions, are not gated either.
func TestAckGateExemptsDeployerFinishAndRefuse(t *testing.T) {
	for _, path := range [][]string{{"claim", "merged", "finish"}, {"claim", "refuse"}} {
		s, task, h, d, entry := releaseFixture(t)
		ctx := context.Background()
		start := time.Now().UTC()
		s.now = func() time.Time { return start }
		if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: h.ID, RunID: h.RunID, To: d.ID, RequestID: "to-deployer", Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: d.Name, Subject: "Please report the release journal state", Body: api.EnvelopeBody{Ask: "report"}}}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
			t.Fatal(err)
		}
		s.now = func() time.Time { return start.Add(api.ObligationAckGrace + time.Minute) }
		// Claim freshness reads the store clock: the deployer heartbeats at it.
		if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(s.now()), d.ID); err != nil {
			t.Fatal(err)
		}
		j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range path {
			req := api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}
			if op == "finish" {
				req.Receipt = &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{}}
			}
			if j, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
				t.Fatalf("%s gated: %v", op, err)
			}
		}
		if want := map[string]string{"finish": "released", "refuse": "refused"}[path[len(path)-1]]; j.State != want {
			t.Fatal(j.State, want)
		}
	}
}
