package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestVerificationDeliverySummaryAndNewPlanClearing(t *testing.T) {
	f, h, p, _ := knownVerificationFixture(t)
	// Seed only the isolated delivery row; item admission already created its live team.
	q := api.TeamQueueEntry{ID: api.NewID("tqe")}
	_, err := f.s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,host,cwd,created_at,updated_at) VALUES(?,?,?,1,?,'planned',1,'running','fixture','/tmp','fixture','fixture')`, q.ID, f.task.ID, f.item.ID, p.OrderMessageSeq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "summary-plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
		t.Fatal(err)
	}
	r := retryVerification(p, 1, 0)
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "summary-receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); err != nil {
		t.Fatal(err)
	}
	lists := []api.TeamQueueList{}
	all, err := f.s.ListTeamQueue(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	lists = append(lists, all)
	host, err := f.s.TeamQueuesByHost(f.ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	lists = append(lists, host)
	entry, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	lists = append(lists, api.TeamQueueList{Entries: []api.TeamQueueEntry{entry}})
	for _, list := range lists {
		v := list.Entries[0].Verification
		if v == nil || v.State != "passing" || len(v.Checks) != 1 || v.Checks[0].Status != "flaky" || !v.Checks[0].KnownFailure || !v.Checks[0].NowPassing {
			t.Fatalf("native summary lost: %+v", v)
		}
	}
	if path := os.Getenv("VERIFICATION_DELIVERY_FIXTURE_OUTPUT"); path != "" {
		raw, _ := json.Marshal(entry.Verification)
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	p.OperationKey = "replacement-plan"
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "replace", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 2, Plan: &p}); err != nil {
		t.Fatal(err)
	}
	entry, err = f.s.GetTeamQueueEntry(f.ctx, f.task.ID, q.ID)
	if err != nil || entry.Verification.State != "pending" || len(entry.Verification.Checks) != 0 {
		t.Fatal("old receipt displayed on new plan", entry.Verification, err)
	}
	tx, _ := f.s.db.BeginTx(f.ctx, nil)
	err = verificationReady(f.ctx, tx, f.item, p.Commit)
	tx.Rollback()
	if err == nil {
		t.Fatal("old receipt allowed completion after new plan")
	}
	// A valid unlisted exhausted failure is stored and displayed as blocked.
	p.OperationKey = "unlisted-plan"
	p.KnownFailures = nil
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "unlisted-plan", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 3, Plan: &p}); err != nil {
		t.Fatal(err)
	}
	failed := retryVerification(p, 1, 1, 1)
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "unlisted-receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 4, Receipt: &failed}); err != nil {
		t.Fatal(err)
	}
	entry, err = f.s.GetTeamQueueEntry(f.ctx, f.task.ID, q.ID)
	if err != nil || entry.Verification.State != "blocked" || entry.Verification.Checks[0].Status != "fail" || entry.Verification.Checks[0].KnownFailure {
		t.Fatal("unlisted failure summary", entry.Verification, err)
	}
	// Scope changes also clear labels instead of presenting stale evidence as passing.
	desc := "new scope"
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Description: &desc}, f.by); err != nil {
		t.Fatal(err)
	}
	entry, err = f.s.GetTeamQueueEntry(f.ctx, f.task.ID, q.ID)
	if err != nil || entry.Verification.State != "stale" || len(entry.Verification.Checks) != 0 {
		t.Fatal("stale summary", entry.Verification, err)
	}
}

// sharedCheckoutFixture is a parallel project with two live handlers and n
// items, each with a bounded order.
func sharedCheckoutFixture(t *testing.T, n int) (*Store, api.Task, []api.WorkItem, []api.Message) {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	for i := len(items); i < n; i++ {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Queued work", Priority: "normal", RequestID: api.NewID("req")}, by)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
		orders = append(orders, contextLinkedMessage(t, s, task, item, "bounded order", api.NewID("req"), nil))
	}
	aux, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "aux-handler", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "aux-handler"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), aux.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "checkout-policy", Operation: "set_host_policy", Host: "mini", HostPolicyVersion: 1, HostPolicyExpires: s.now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: "https://fixture.invalid", HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20}); err != nil {
		t.Fatal(err)
	}
	syntheticHostUsage(t, s, task.ID, "mini")
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "checkout-limit", Operation: "set_limit", Host: "mini", ConcurrencyLimit: 2}); err != nil {
		t.Fatal(err)
	}
	return s, task, items, orders
}

const checkoutBase = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func addCheckoutEntry(t *testing.T, s *Store, task api.Task, item api.WorkItem, order api.Message, cwd string, owns ...string) api.TeamQueueEntry {
	t.Helper()
	q, err := s.TeamQueueAction(context.Background(), task.ID, api.TeamQueueRequest{RequestID: "add-" + item.ID, Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: cwd, Repository: "repo", BaseCommit: checkoutBase, Ownership: owns})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestTeamQueueScopeMovesSharedCheckoutEntryToItsOwnWorktree(t *testing.T) {
	s, task, items, orders := sharedCheckoutFixture(t, 4)
	ctx := context.Background()
	a := addCheckoutEntry(t, s, task, items[0], orders[0], "/main", "src/a")
	b := addCheckoutEntry(t, s, task, items[1], orders[1], "/main", "src/b")
	c := addCheckoutEntry(t, s, task, items[2], orders[2], "/other", "src/a/child")
	d := addCheckoutEntry(t, s, task, items[3], orders[3], "/main", "src/a/other")
	a, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-a", Operation: "claim", EntryID: a.ID, ExpectedRevision: a.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	// B conflicts with A only by its checkout; C by its ownership; D by both.
	got := listedEntry(t, s, task.ID, b.ID)
	if len(got.BlockedBy) != 1 || got.BlockedBy[0] != a.ID || !strings.Contains(got.BlockReason, "Shares checkout /main") || !strings.Contains(got.BlockReason, a.ID) || !strings.Contains(got.BlockReason, "--entry "+b.ID+" --new-worktree") {
		t.Fatalf("shared checkout reason %q blocked by %v", got.BlockReason, got.BlockedBy)
	}
	if got := listedEntry(t, s, task.ID, c.ID); len(got.BlockedBy) != 1 || got.BlockedBy[0] != a.ID || strings.Contains(got.BlockReason, "Shares checkout") {
		t.Fatalf("ownership overlap reason %q blocked by %v", got.BlockReason, got.BlockedBy)
	}
	// Moving D would not free it: it overlaps A too.
	if got := listedEntry(t, s, task.ID, d.ID); len(got.BlockedBy) != 1 || got.BlockedBy[0] != a.ID || strings.Contains(got.BlockReason, "Shares checkout") {
		t.Fatalf("overlap and shared checkout reason %q blocked by %v", got.BlockReason, got.BlockedBy)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-b-shared", Operation: "claim", EntryID: b.ID, ExpectedRevision: b.Revision, Host: "mini"}); err == nil {
		t.Fatal("B launched in A's checkout")
	}
	move := api.TeamQueueRequest{RequestID: "move-b", Operation: "scope", EntryID: b.ID, ExpectedRevision: b.Revision, Ownership: b.Ownership, Cwd: "/main/.build/worktrees/queue-b", Repository: "repo"}
	moved, err := s.TeamQueueAction(ctx, task.ID, move)
	if err != nil {
		t.Fatal(err)
	}
	want := b
	want.Cwd, want.Revision, want.UpdatedAt = move.Cwd, b.Revision+1, moved.UpdatedAt
	if moved.ID != want.ID || moved.Position != want.Position || moved.ItemID != want.ItemID || moved.ItemRevision != want.ItemRevision || moved.OrderMessageSeq != want.OrderMessageSeq || moved.Repository != want.Repository || moved.BaseCommit != want.BaseCommit || moved.Cwd != want.Cwd || moved.Revision != want.Revision || moved.State != "queued" || moved.Serial || strings.Join(moved.Ownership, ",") != "src/b" {
		t.Fatalf("moved entry %+v, want %+v", moved, want)
	}
	if replay, err := s.TeamQueueAction(ctx, task.ID, move); err != nil || replay.Revision != moved.Revision || replay.Cwd != moved.Cwd {
		t.Fatalf("replay %+v %v", replay, err)
	}
	if saved, err := s.GetTeamQueueEntry(ctx, task.ID, b.ID); err != nil || saved.Cwd != move.Cwd || saved.Revision != moved.Revision || saved.Position != b.Position {
		t.Fatalf("saved %+v %v", saved, err)
	}
	if got := listedEntry(t, s, task.ID, b.ID); len(got.BlockedBy) != 0 || strings.Contains(got.BlockReason, "Shares checkout") {
		t.Fatalf("moved B still blocked: %q %v", got.BlockReason, got.BlockedBy)
	}
	claimed, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-b-own", Operation: "claim", EntryID: b.ID, ExpectedRevision: moved.Revision, Host: "mini"})
	if err != nil {
		t.Fatalf("moved B did not launch beside A: %v", err)
	}
	if claimed.Cwd != move.Cwd || claimed.State != "launching" {
		t.Fatalf("claimed B %+v", claimed)
	}
	if still, err := s.GetTeamQueueEntry(ctx, task.ID, a.ID); err != nil || still.State != "launching" || still.Cwd != "/main" || still.HandlerID == claimed.HandlerID {
		t.Fatalf("A beside B: %+v %v (B handler %s)", still, err, claimed.HandlerID)
	}
}

func TestTeamQueueScopeWorktreeRefusalsLeaveEntryUnchanged(t *testing.T) {
	s, task, items, orders := sharedCheckoutFixture(t, 4)
	ctx := context.Background()
	a := addCheckoutEntry(t, s, task, items[0], orders[0], "/main", "src/a")
	b := addCheckoutEntry(t, s, task, items[1], orders[1], "/main", "src/b")
	d := addCheckoutEntry(t, s, task, items[2], orders[2], "/d", "src/d")
	addCheckoutEntry(t, s, task, items[3], orders[3], "/e", "src/e")
	// Claims go in queue order: A, then D (B shares A's checkout).
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-a", Operation: "claim", EntryID: a.ID, ExpectedRevision: a.Revision, Host: "mini"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-d", Operation: "claim", EntryID: d.ID, ExpectedRevision: d.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	move := func(q api.TeamQueueEntry, key, cwd, repository string) error {
		_, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "scope", EntryID: q.ID, ExpectedRevision: q.Revision, Ownership: q.Ownership, Cwd: cwd, Repository: repository})
		return err
	}
	for _, c := range []struct{ name, cwd, repository string }{
		{"relative", "main/.build/worktrees/queue-b", "repo"},
		{"unclean", "/main/../worktrees/queue-b", "repo"},
		{"trailing-slash", "/main/worktrees/queue-b/", "repo"},
		{"nul", "/main/worktrees/queue\x00b", "repo"},
		{"other-repository", "/main/worktrees/queue-b", "other"},
		{"no-repository", "/main/worktrees/queue-b", ""},
		{"active-cwd", "/d", "repo"},
		{"queued-cwd", "/e", "repo"},
	} {
		if err := move(b, "move-b-"+c.name, c.cwd, c.repository); err == nil {
			t.Fatalf("%s: move accepted", c.name)
		}
		if got, err := s.GetTeamQueueEntry(ctx, task.ID, b.ID); err != nil || got.Cwd != "/main" || got.Revision != b.Revision {
			t.Fatalf("%s: entry changed %+v %v", c.name, got, err)
		}
	}
	if err := move(d, "move-launching-d", "/d-own", "repo"); err == nil || !strings.Contains(err.Error(), "only a queued entry") {
		t.Fatalf("launching entry moved: %v", err)
	}
	failed, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "fail-d", Operation: "fail", EntryID: d.ID, ExpectedRevision: d.Revision, Failure: "synthetic failure"})
	if err != nil {
		t.Fatal(err)
	}
	if err := move(failed, "move-failed-d", "/d-own", "repo"); err == nil || !strings.Contains(err.Error(), "only a queued entry") {
		t.Fatalf("failed entry moved: %v", err)
	}
	if got, err := s.GetTeamQueueEntry(ctx, task.ID, d.ID); err != nil || got.Cwd != "/d" || got.Revision != failed.Revision {
		t.Fatalf("failed entry changed %+v %v", got, err)
	}
	// A plain scope keeps the checkout.
	scoped, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "narrow-b", Operation: "scope", EntryID: b.ID, ExpectedRevision: b.Revision, Ownership: []string{"src/b/only"}})
	if err != nil || scoped.Cwd != "/main" || strings.Join(scoped.Ownership, ",") != "src/b/only" {
		t.Fatalf("plain scope %+v %v", scoped, err)
	}
}

func TestTeamQueueScopeWorktreeNeedsARepository(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	// A serial project admits a legacy entry without a frozen repository.
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add-legacy", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/main", Ownership: []string{"src/a"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range []string{"", "repo"} {
		if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "move-legacy-" + repository, Operation: "scope", EntryID: q.ID, ExpectedRevision: q.Revision, Ownership: q.Ownership, Cwd: "/main/worktrees/queue-a", Repository: repository}); err == nil || !strings.Contains(err.Error(), "entry's repository") {
			t.Fatalf("repository %q: %v", repository, err)
		}
	}
	if got, err := s.GetTeamQueueEntry(ctx, task.ID, q.ID); err != nil || got.Cwd != "/main" || got.Revision != q.Revision {
		t.Fatalf("legacy entry changed %+v %v", got, err)
	}
}
