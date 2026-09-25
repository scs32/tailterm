package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func queueFixture(t *testing.T) (*Store, api.Task, []api.WorkItem, []api.Message) {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Queue fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	var items []api.WorkItem
	var orders []api.Message
	for i := 0; i < 2; i++ {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Queued work", Priority: "normal", RequestID: api.NewID("req")}, by)
		if err != nil {
			t.Fatal(err)
		}
		order := contextLinkedMessage(t, s, task, item, "bounded order", api.NewID("req"), nil)
		items = append(items, item)
		orders = append(orders, order)
	}
	handler, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "database", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "database"}, by)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), handler.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, items, orders
}

func TestTeamQueueTwoRunnersAndManualLaunchRace(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: []string{"runner-one", "runner-two"}[i], Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("runner claims=%d", success)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "manual-other", Operation: "manual", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq}); err == nil {
		t.Fatal("manual launch crossed runner claim")
	}
	name := "other-lead"
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &name}, api.Caller{Node: "fixture", User: "owner"}); err == nil {
		t.Fatal("unreserved lead assignment crossed runner claim")
	}
}

func TestTeamQueueClaimRefusesUnavailableHandlerLiveLeadAndPause(t *testing.T) {
	for _, condition := range []string{"offline-handler", "live-lead", "paused"} {
		t.Run(condition, func(t *testing.T) {
			s, task, items, orders := queueFixture(t)
			ctx := context.Background()
			q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
			if err != nil {
				t.Fatal(err)
			}
			switch condition {
			case "offline-handler":
				_, err = s.db.Exec(`UPDATE agents SET last_seen_at='' WHERE task_id=? AND role=?`, task.ID, api.AgentRoleDatabaseHandler)
			case "live-lead":
				name := "manual-lead"
				_, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &name}, api.Caller{Node: "fixture", User: "owner"})
			case "paused":
				_, err = s.db.Exec(`UPDATE tasks SET pause_state='paused',pause_generation=pause_generation+1 WHERE id=?`, task.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"}); err == nil {
				t.Fatal("unsafe claim accepted")
			}
		})
	}
}

func TestTeamQueueReopensWithSavedEntry(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	list, err := reopened.ListTeamQueue(ctx, task.ID)
	if err != nil || len(list.Entries) != 1 || list.Entries[0].ID != q.ID {
		t.Fatalf("reopened queue %+v %v", list, err)
	}
}

func TestTeamQueuePersistsOrderAndRefusesBadOrders(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	add := func(i int, request string) api.TeamQueueEntry {
		t.Helper()
		q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: request, Operation: "add", ItemID: items[i].ID, OrderMessageSeq: orders[i].Seq, Template: "planned", Host: "mini", Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "missing-order", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: 999999, Host: "mini", Cwd: "/tmp"}); err == nil {
		t.Fatal("missing order accepted")
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wrong-order", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[1].Seq, Host: "mini", Cwd: "/tmp"}); err == nil {
		t.Fatal("wrong order accepted")
	}
	q1, q2 := add(0, "add-one"), add(1, "add-two")
	if replay, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add-one", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Template: "planned", Host: "mini", Cwd: "/tmp"}); err != nil || replay.ID != q1.ID {
		t.Fatalf("retry %+v %v", replay, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "duplicate", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"}); err == nil {
		t.Fatal("duplicate item accepted")
	}
	q2, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "reorder-two", Operation: "reorder", EntryID: q2.ID, ExpectedRevision: q2.Revision, BeforeID: q1.ID})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTeamQueue(ctx, task.ID)
	if err != nil || len(list.Entries) != 2 || list.Entries[0].ID != q2.ID {
		t.Fatalf("reorder %+v %v", list, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "stale", Operation: "remove", EntryID: q2.ID, ExpectedRevision: 1}); err == nil {
		t.Fatal("stale revision accepted")
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "remove-two", Operation: "remove", EntryID: q2.ID, ExpectedRevision: q2.Revision}); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListTeamQueue(ctx, task.ID)
	if err != nil || len(list.Entries) != 1 || list.Entries[0].ID != q1.ID {
		t.Fatalf("remove %+v %v", list, err)
	}
}

func TestTeamQueueClaimAttemptFailureIsFrozen(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wrong-host", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "other"}); err == nil {
		t.Fatal("wrong host claimed")
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "manual", Operation: "manual", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq}); err == nil {
		t.Fatal("manual launch crossed reservation")
	}
	plan, _ := json.Marshal(map[string]any{"task": task.ID, "item": items[0].ID, "revision": items[0].Revision, "order": orders[0].Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": api.NewID("run"), "fields": map[string]any{"agentId": api.NewID("agt"), "name": "lead", "cwd": "/tmp"}}}})
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "freeze", Operation: "freeze", EntryID: q.ID, ExpectedRevision: q.Revision, LaunchJSON: plan})
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "attempt-again", Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision}); err == nil {
		t.Fatal("uncertain spawn reattempted")
	}
	failure := api.TeamQueueRequest{RequestID: "fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "uncertain exact session"}
	q, err = s.TeamQueueAction(ctx, task.ID, failure)
	if err != nil || q.EscalationSeq < 1 {
		t.Fatalf("failure %+v %v", q, err)
	}
	if again, err := s.TeamQueueAction(ctx, task.ID, failure); err != nil || again.EscalationSeq != q.EscalationSeq {
		t.Fatalf("failure replay %+v %v", again, err)
	}
	messages, err := s.ListMessages(ctx, task.ID, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, message := range messages {
		if message.Seq == q.EscalationSeq {
			notices++
			if message.Envelope == nil || message.Envelope.Kind != api.EnvelopeKindNotice || message.Envelope.Refs["escalation"] != "owner" {
				t.Fatalf("untyped owner notice %+v", message)
			}
		}
	}
	if notices != 1 {
		t.Fatalf("owner notices=%d", notices)
	}
}

func TestTeamQueueFailedReleaseKeepsHistoryAndAllowsNextClaim(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	first, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add-one", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add-two", Operation: "add", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	first, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-one", Operation: "claim", EntryID: first.ID, ExpectedRevision: first.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	first, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "fail-one", Operation: "fail", EntryID: first.ID, ExpectedRevision: first.Revision, Failure: "planned fixture failure"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "early-claim", Operation: "claim", EntryID: second.ID, ExpectedRevision: second.Revision, Host: "mini"}); err == nil {
		t.Fatal("unreleased failure allowed claim")
	}
	release := api.TeamQueueRequest{RequestID: "release-one", Operation: "release", EntryID: first.ID, ExpectedRevision: first.Revision}
	first, err = s.TeamQueueAction(ctx, task.ID, release)
	if err != nil || first.ReleasedAt == "" || first.State != "failed" {
		t.Fatalf("release %+v %v", first, err)
	}
	if again, err := s.TeamQueueAction(ctx, task.ID, release); err != nil || again.ReleasedAt != first.ReleasedAt {
		t.Fatalf("release receipt %+v %v", again, err)
	}
	release.ExpectedRevision = first.Revision
	if again, err := s.TeamQueueAction(ctx, task.ID, release); err != nil || again.ReleasedAt != first.ReleasedAt {
		t.Fatalf("release after response loss %+v %v", again, err)
	}
	second, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-two", Operation: "claim", EntryID: second.ID, ExpectedRevision: second.Revision, Host: "mini"})
	if err != nil || second.State != "launching" {
		t.Fatalf("next claim %+v %v", second, err)
	}
	stored, err := s.GetTeamQueueEntry(ctx, task.ID, first.ID)
	if err != nil || stored.State != "failed" || stored.EscalationSeq == 0 || stored.ReleasedAt == "" {
		t.Fatalf("history %+v %v", stored, err)
	}
}

func TestTeamQueueReleaseRejectsUncertainRunAndUnsafeProject(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(map[string]any{"task": task.ID, "item": items[0].ID, "revision": items[0].Revision, "order": orders[0].Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": api.NewID("run"), "fields": map[string]any{"agentId": api.NewID("agt"), "name": "lead", "cwd": "/tmp"}}}})
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "freeze", Operation: "freeze", EntryID: q.ID, ExpectedRevision: q.Revision, LaunchJSON: plan})
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision})
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "uncertain spawn"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "release-uncertain", Operation: "release", EntryID: q.ID, ExpectedRevision: q.Revision}); err == nil {
		t.Fatal("unresolved uncertain spawn released")
	}
	var frozen struct {
		Members []struct {
			Fields struct {
				AgentID string `json:"agentId"`
				Name    string `json:"name"`
			} `json:"fields"`
			RunID string `json:"runId"`
		} `json:"members"`
	}
	if err := json.Unmarshal(q.LaunchJSON, &frozen); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(q.LaunchJSON)
	proof := &api.TeamQueueReleaseProof{TaskID: task.ID, EntryID: q.ID, ItemID: items[0].ID, Host: "mini", LaunchDigest: hex.EncodeToString(digest[:]), Members: []api.TeamQueueReleaseMember{{AgentID: frozen.Members[0].Fields.AgentID, RunID: api.NewID("run"), Name: frozen.Members[0].Fields.Name}}}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wrong-run-proof", Operation: "release", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini", SessionsChecked: true, ReleaseProof: proof}); err == nil {
		t.Fatal("wrong saved run proof released")
	}
	proof.Members[0].RunID = frozen.Members[0].RunID
	proof.LaunchDigest = "wrong"
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wrong-digest-proof", Operation: "release", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini", SessionsChecked: true, ReleaseProof: proof}); err == nil {
		t.Fatal("stale frozen plan proof released")
	}
	if _, err := s.db.Exec(`UPDATE tasks SET orchestrator='someone' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "release-live", Operation: "release", EntryID: q.ID, ExpectedRevision: q.Revision}); err == nil {
		t.Fatal("live lead released")
	}
}

func TestTeamQueueAbandonManualReservationHasExactReceipt(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	token := "manual-" + task.ID + "-" + items[0].ID + "-" + fmt.Sprint(orders[0].Seq)
	_, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: token, Operation: "manual", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, PauseGeneration: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wrong-token", Operation: "manual_release", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, ReservationToken: "manual-wrong"}); err == nil {
		t.Fatal("wrong token released")
	}
	release := api.TeamQueueRequest{RequestID: "manual-release", Operation: "manual_release", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, ReservationToken: token}
	got, err := s.TeamQueueAction(ctx, task.ID, release)
	if err != nil || got.ReleasedAt == "" {
		t.Fatalf("manual release %+v %v", got, err)
	}
	if again, err := s.TeamQueueAction(ctx, task.ID, release); err != nil || again.ReleasedAt != got.ReleasedAt {
		t.Fatalf("manual receipt %+v %v", again, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: token, Operation: "manual", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, PauseGeneration: 0}); err == nil {
		t.Fatal("released reservation replayed as an active launch")
	}
	_, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "manual-next", Operation: "manual", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq, PauseGeneration: 0})
	if err != nil {
		t.Fatalf("manual next %v", err)
	}
}

func TestTeamQueueManualPostLeadReleaseNeedsExactResolvedProof(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	token := fmt.Sprintf("manual-%s-%s-%d", task.ID, items[0].ID, orders[0].Seq)
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: token, Operation: "manual", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, PauseGeneration: 0}); err != nil {
		t.Fatal(err)
	}
	lead := "manual-lead"
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &lead, TeamLaunchToken: token}, by); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &empty}, by); err != nil {
		t.Fatal(err)
	}
	release := api.TeamQueueRequest{RequestID: "resolved-manual", Operation: "manual_release", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, ReservationToken: token}
	if _, err := s.TeamQueueAction(ctx, task.ID, release); err == nil {
		t.Fatal("post-lead manual reservation released without proof")
	}
	proof, _ := json.Marshal(map[string]any{"task": task.ID, "item": items[0].ID, "order": orders[0].Seq, "members": []any{map[string]any{"agentId": api.NewID("agt"), "state": "uncertain", "runId": ""}}})
	release.ManualJournal = proof
	release.SessionsChecked = true
	if _, err := s.TeamQueueAction(ctx, task.ID, release); err != nil {
		t.Fatalf("verified absence release: %v", err)
	}
}

func TestTeamQueueReleaseWaitsForRegisteredRunCleanup(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	ref := api.MessageReference{TaskID: task.ID, Seq: orders[0].Seq}
	bundle := syntheticPreparedContext(t, items[0], ref, syntheticHistory(items[0], orders[0]))
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "partial-member", Host: "mini", Session: "partial-member", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: items[0].ID, ItemRevision: items[0].Revision, WorkOrderMessage: ref, ContextBundle: bundle}}, by)
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "partial launch"})
	if err != nil {
		t.Fatal(err)
	}
	release := api.TeamQueueRequest{RequestID: "release", Operation: "release", EntryID: q.ID, ExpectedRevision: q.Revision}
	if _, err := s.TeamQueueAction(ctx, task.ID, release); err == nil {
		t.Fatal("live run released")
	}
	if _, err := s.CloseAgent(ctx, a.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, release); err == nil {
		t.Fatal("uncleaned run released")
	}
	if _, err := s.ReportCleanup(ctx, a.ID, api.CleanupRequest{RunID: a.RunID}, by); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, release); err != nil {
		t.Fatalf("cleaned run release: %v", err)
	}
}

func TestManualReservationSameItemLeadReplacementKeepsSafety(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	add := func(name string, i int) api.Agent {
		ref := api.MessageReference{TaskID: task.ID, Seq: orders[i].Seq}
		bundle := syntheticPreparedContext(t, items[i], ref, syntheticHistory(items[i], orders[i]))
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "mini", Session: name, Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: items[i].ID, ItemRevision: items[i].Revision, WorkOrderMessage: ref, ContextBundle: bundle}}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	wrong := add("wrong-item", 1)
	token := fmt.Sprintf("manual-%s-%s-%d", task.ID, items[0].ID, orders[0].Seq)
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: token, Operation: "manual", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, PauseGeneration: 0}); err != nil {
		t.Fatal(err)
	}
	lead := "manual-lead"
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &lead, TeamLaunchToken: token}, by); err != nil {
		t.Fatal(err)
	}
	add(lead, 0)
	second := add("manual-replacement", 0)
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &second.Name}, by); err != nil {
		t.Fatalf("same-item manual replacement: %v", err)
	}
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &wrong.Name}, by); err == nil {
		t.Fatal("wrong-item manual replacement accepted")
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "second-manual", Operation: "manual", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq, PauseGeneration: 0}); err == nil {
		t.Fatal("duplicate manual launch accepted")
	}
	if _, err := s.db.Exec(`UPDATE tasks SET pause_generation=pause_generation+1 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &lead}, by); err == nil {
		t.Fatal("stale pause-generation replacement accepted")
	}
}
