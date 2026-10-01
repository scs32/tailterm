package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// A stalled active entry that only shares a queued entry's checkout keeps the
// move hint in the Stalled reason, which the stall notice carries.
func TestTeamQueueStalledSharedCheckoutKeepsMoveHint(t *testing.T) {
	s, task, items, orders := sharedCheckoutFixture(t, 4)
	ctx := context.Background()
	s.queueStallGrace = time.Nanosecond // A has had no runs for the whole grace
	a := addCheckoutEntry(t, s, task, items[0], orders[0], "/main", "src/a")
	b := addCheckoutEntry(t, s, task, items[1], orders[1], "/main", "src/b")
	c := addCheckoutEntry(t, s, task, items[2], orders[2], "/other", "src/a/child")
	d := addCheckoutEntry(t, s, task, items[3], orders[3], "/main", "src/a/other")
	a, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-a", Operation: "claim", EntryID: a.ID, ExpectedRevision: a.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	got := listedEntry(t, s, task.ID, b.ID)
	if got.Stall == nil || got.Stall.BlockerEntryID != a.ID || !strings.HasPrefix(got.BlockReason, "Stalled: "+a.ID) || !strings.Contains(got.BlockReason, "Or: Shares checkout /main with active entry "+a.ID+"; move it: tt team queue scope --task "+task.ID+" --entry "+b.ID+" --new-worktree") {
		t.Fatalf("stalled shared checkout: %+v reason %q", got.Stall, got.BlockReason)
	}
	for _, id := range []string{c.ID, d.ID} {
		got := listedEntry(t, s, task.ID, id)
		if got.Stall == nil || strings.Contains(got.BlockReason, "Shares checkout") {
			t.Fatalf("ownership overlap %s: %+v reason %q", id, got.Stall, got.BlockReason)
		}
	}
	// The notice posted for B's stall names the move too.
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: got.Stall.NoticeRequestID(b.ID), Operation: "stall_notice", EntryID: b.ID}); err != nil {
		t.Fatal(err)
	}
	messages, err := s.ListMessages(ctx, task.ID, 0, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range messages {
		if m.Envelope != nil && m.Envelope.Refs["entry"] == b.ID && strings.Contains(m.Envelope.Body.Text, "--entry "+b.ID+" --new-worktree") {
			found = true
		}
	}
	if !found {
		t.Fatal("stall notice lacks the move hint")
	}
}

// paddedLaunch is a frozen launch JSON of about size bytes whose second
// member has the given role.
func paddedLaunch(size int, role string) []byte {
	head := fmt.Sprintf(`{"members":[{"fields":{"name":"lead","role":"Delivery lead and orchestrator"}},{"fields":{"name":"second","role":%q}}],"pad":"`, role)
	return []byte(head + strings.Repeat("x", max(size-len(head)-2, 0)) + `"}`)
}

// insertHistoryEntries adds n work items, each with a finished queue entry
// at positions from..from+n-1 carrying the given launch and close JSON.
func insertHistoryEntries(t *testing.T, s *Store, task api.Task, from, n int, launch, closeJSON []byte) []api.TeamQueueEntry {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	out := make([]api.TeamQueueEntry, 0, n)
	for i := 0; i < n; i++ {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Finished work", Priority: "normal", RequestID: api.NewID("req")}, by)
		if err != nil {
			t.Fatal(err)
		}
		id, now := api.NewID("tqe"), ts(s.now())
		if _, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,launch_json,close_json,created_at,updated_at) VALUES(?,?,?,?,1,'planned',?,'finished',3,'mini','/tmp',?,?,?,?)`, id, task.ID, item.ID, item.Revision, from+i, string(launch), string(closeJSON), now, now); err != nil {
			t.Fatal(err)
		}
		out = append(out, api.TeamQueueEntry{ID: id, ItemID: item.ID, Position: int64(from + i)})
	}
	return out
}

// q1, q4: finished entries with large launch contexts cost a small fixed
// amount in the listing; one entry still reads in full.
func TestTeamQueueListingStaysWithinBudget(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	launch, closeJSON := paddedLaunch(64<<10, "Plan review"), []byte(`{"pad":"`+strings.Repeat("c", 16<<10)+`"}`)
	history := insertHistoryEntries(t, s, task, 1, 200, launch, closeJSON)
	running := api.NewID("tqe")
	if _, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,launch_json,created_at,updated_at) VALUES(?,?,?,?,?,'planned',201,'running',2,'mini','/tmp',?,?,?)`, running, task.ID, items[0].ID, items[0].Revision, orders[0].Seq, string(launch), ts(s.now()), ts(s.now())); err != nil {
		t.Fatal(err)
	}
	queued, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "add-queued", Operation: "add", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq, Host: "mini", Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		opts api.TeamQueueListOptions
		want int
	}{{api.TeamQueueListOptions{}, api.DefaultTeamQueueHistoryLimit}, {api.TeamQueueListOptions{Limit: 200}, 200}} {
		list, err := s.TeamQueuePage(ctx, task.ID, c.opts)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) >= 256<<10 {
			t.Fatalf("limit %d listing is %d bytes, budget 256 KiB", c.opts.Limit, len(raw))
		}
		if len(list.Entries) != 2+c.want || list.Entries[0].ID != running || list.Entries[1].ID != queued.ID {
			t.Fatalf("limit %d: %d entries, active first %s %s", c.opts.Limit, len(list.Entries), list.Entries[0].ID, list.Entries[1].ID)
		}
		if string(list.Entries[0].LaunchJSON) != string(launch) || list.Entries[0].Summary {
			t.Fatal("the running entry's launch was trimmed")
		}
		if list.History == nil || list.History.Total != 200 || list.History.Limit != c.want {
			t.Fatalf("history %+v", list.History)
		}
		for i, e := range list.Entries[2:] {
			if e.ID != history[199-i].ID || !e.Summary || len(e.LaunchJSON) != 0 || len(e.CloseJSON) != 0 || e.Activities != nil || e.TeamShape != "plan-review" {
				t.Fatalf("history entry %d: id %s summary %v launch %d close %d activities %v shape %q", i, e.ID, e.Summary, len(e.LaunchJSON), len(e.CloseJSON), e.Activities, e.TeamShape)
			}
		}
		t.Logf("limit %d: %d bytes", c.opts.Limit, len(raw))
	}
	full, err := s.GetTeamQueueEntry(ctx, task.ID, history[0].ID)
	if err != nil || string(full.LaunchJSON) != string(launch) || string(full.CloseJSON) != string(closeJSON) || full.Summary {
		t.Fatalf("full entry: launch %d close %d summary %v %v", len(full.LaunchJSON), len(full.CloseJSON), full.Summary, err)
	}
	// The unpaged internal listing is what outgrew the 4 MiB client cap.
	all, err := s.ListTeamQueue(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(all); len(raw) < 4<<20 {
		t.Fatalf("full listing %d bytes; the fixture no longer reproduces the incident", len(raw))
	}
}

// q1: one history entry with every heavy field summarizes to a bounded size
// that keeps what the Delivery view and tt read.
func TestTeamQueueSummaryBoundsHeavyFields(t *testing.T) {
	big := func(n int) string { return strings.Repeat("y", n) }
	e := api.TeamQueueEntry{ID: "tqe_heavy", TaskID: "tsk_heavy", ItemID: "wi_heavy", State: "finished", Position: 7, Revision: 9, Host: "mini", Cwd: "/w", Repository: "repo", Ownership: []string{"hub/a", "hub/b"}, HandlerID: "agt_h", HandlerRunID: "run_h", HandlerLeaseGeneration: 2, ReleasedAt: "2026-09-30T00:00:00Z", Tokens: api.TokenTotals{Input: 11, Total: 42}}
	e.LaunchJSON = paddedLaunch(1<<20, "Implementation")
	e.CloseJSON = []byte(`{"pad":"` + big(256<<10) + `"}`)
	for i := 0; i < 10; i++ {
		e.Activities = append(e.Activities, api.TeamAgentActivity{AgentID: fmt.Sprint("agt_", i), RunID: "run", Name: big(64), Activity: &api.AgentActivity{State: "idle", Reason: big(1024)}})
	}
	reviews := api.ReviewConvergence{ItemID: e.ItemID, History: "recorded", Disposition: &api.ReviewDisposition{Kind: "accepted", Candidate: "abc", MessageSeq: 5}}
	for r := 1; r <= 5; r++ {
		round := api.ReviewRound{Number: r, RequestSeq: int64(100 + r), Candidate: "abc", ReviewerID: "agt_r", Criteria: map[string]string{"a1": big(2048)}, VerificationCriteria: []string{"a1"}}
		for f := 0; f < 20; f++ {
			round.Findings = append(round.Findings, api.ReviewFinding{ID: fmt.Sprint("F", f), Title: big(128), Description: big(4096)})
		}
		round.Blockers = round.Findings[:5]
		reviews.Rounds = append(reviews.Rounds, round)
		reviews.Scopes = append(reviews.Scopes, api.ReviewScope{ScopeRevision: int64(r), Criteria: map[string]string{"a1": big(2048)}})
		reviews.Focused = append(reviews.Focused, api.FocusedReview{RequestSeq: int64(r), Fix: big(2048)})
	}
	for f := 0; f < 10; f++ {
		reviews.FollowUps = append(reviews.FollowUps, api.ReviewFollowUp{ItemID: fmt.Sprint("wi_follow", f), MessageSeq: int64(f), Finding: api.ReviewFinding{ID: fmt.Sprint("U", f), Title: "é" + big(1024), Description: big(4096)}})
	}
	e.Reviews = &reviews
	verification := api.VerificationSummary{State: "passing", Commit: "abc"}
	for i := 0; i < 200; i++ {
		verification.Checks = append(verification.Checks, api.VerificationCheckSummary{ID: fmt.Sprint("check-", i), Status: "pass"})
	}
	verification.Checks[3].Status = "fail"
	verification.Checks[50].KnownFailure = true
	verification.Checks[150].NowPassing = true
	e.Verification = &verification
	plan := api.VerificationPlan{ItemID: e.ItemID, Commit: "abc"}
	for i := 0; i < 200; i++ {
		plan.Checks = append(plan.Checks, api.VerificationCheck{ID: fmt.Sprint("check-", i), Argv: []string{"go", "test", big(64)}, Cwd: "/w"})
	}
	receipt := api.VerificationReceipt{Commit: "abc"}
	for i := 0; i < 200; i++ {
		receipt.Checks = append(receipt.Checks, api.VerificationResult{Status: "pass", VerificationCheck: plan.Checks[i]})
	}
	e.Release = &api.ReleaseJob{ID: "rel_heavy", State: "released", Commit: "abc", VerificationDigest: "d", Plan: plan, IntegratedPlan: &plan, IntegratedVerification: &receipt, Reconciliations: []api.ReleaseReconciliation{{CausalEvidence: big(4096)}}, Receipt: &api.ReleaseReceipt{JobID: "rel_heavy", Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "hub", Outcome: "deployed", Release: "r1"}}}}
	e.Acceptance = &api.TeamIntegrationAcceptance{Branch: "b", Commit: "abc", Evidence: big(2000)}
	e.Integration = &api.TeamIntegrationReady{Branch: "b", Commit: "abc", Evidence: big(2000)}
	e.OwnerIntegration = &api.TeamQueueOwnerIntegration{Commit: "abc", Evidence: big(2000)}
	for i := 0; i < 4096; i++ {
		e.OwnerIntegration.ChangedFiles = append(e.OwnerIntegration.ChangedFiles, fmt.Sprintf("hub/internal/file_%04d.go", i))
	}
	if raw, _ := json.Marshal(e); len(raw) < 1<<20 {
		t.Fatalf("fixture is only %d bytes", len(raw))
	}
	sum := summarizeTeamQueueEntry(e)
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 8<<10 {
		t.Fatalf("summary is %d bytes, budget 8 KiB: %s", len(raw), raw)
	}
	t.Logf("summary: %d bytes", len(raw))
	if !sum.Summary || sum.TeamShape != "plan-only" || sum.ID != e.ID || sum.ItemID != e.ItemID || sum.State != e.State || sum.Position != e.Position || sum.Revision != e.Revision || sum.Tokens != e.Tokens || strings.Join(sum.Ownership, ",") != "hub/a,hub/b" || sum.HandlerID != e.HandlerID || sum.ReleasedAt != e.ReleasedAt {
		t.Fatalf("identity lost: %s", raw)
	}
	if len(sum.LaunchJSON) != 0 || len(sum.CloseJSON) != 0 || sum.Activities != nil {
		t.Fatal("launch, close or activities kept")
	}
	if r := sum.Release; r == nil || r.ID != "rel_heavy" || r.State != "released" || r.Receipt == nil || r.Receipt.Targets[0].Release != "r1" || len(r.Plan.Checks) != 0 || r.IntegratedPlan != nil || r.IntegratedVerification != nil || r.Reconciliations != nil {
		t.Fatalf("release summary %+v", sum.Release)
	}
	rv := sum.Reviews
	if rv == nil || rv.History != "recorded" || rv.Disposition == nil || rv.Disposition.Kind != "accepted" || len(rv.Rounds) != 5 || len(rv.Scopes) != 0 || len(rv.Focused) != 0 || len(rv.FollowUps) != 10 {
		t.Fatalf("review summary %+v", rv)
	}
	for i, round := range rv.Rounds {
		if round.Number != i+1 || round.Findings != nil || round.Criteria != nil || round.Blockers != nil {
			t.Fatalf("round %d %+v", i, round)
		}
	}
	for i, f := range rv.FollowUps {
		if f.ItemID != fmt.Sprint("wi_follow", i) || len(f.Finding.Title) > summaryTitleBytes || !strings.HasPrefix(f.Finding.Title, "é") || f.Finding.Description != "" {
			t.Fatalf("follow-up %d %+v", i, f)
		}
	}
	if v := sum.Verification; v == nil || v.State != "passing" || len(v.Checks) != 3 || v.Checks[0].ID != "check-3" || v.Checks[1].ID != "check-50" || v.Checks[2].ID != "check-150" {
		t.Fatalf("verification summary %+v", sum.Verification)
	}
	if len(sum.OwnerIntegration.ChangedFiles) != summaryChangedFiles || len(sum.Acceptance.Evidence) > summaryEvidenceBytes || len(sum.Integration.Evidence) > summaryEvidenceBytes || len(sum.OwnerIntegration.Evidence) > summaryEvidenceBytes {
		t.Fatal("evidence or changed files not bounded")
	}
	// The source entry is untouched.
	if len(e.Reviews.Rounds[0].Findings) != 20 || len(e.Release.Plan.Checks) != 200 || len(e.OwnerIntegration.ChangedFiles) != 4096 || len(e.Verification.Checks) != 200 {
		t.Fatal("summarizing changed the source entry")
	}
}

// q3: history pages newest first by a stable position cursor, behind the
// active entries, while entries finish between pages.
func TestTeamQueuePageHistoryPaging(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	history := insertHistoryEntries(t, s, task, 1, 120, []byte(`{}`), nil)
	// A released failed entry is history; an owner-integrated failed one is
	// still active, because the runner must close its team.
	extra := insertHistoryEntries(t, s, task, 121, 2, []byte(`{}`), nil)
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='failed',released_at='2026-09-30T00:00:00Z' WHERE id=?`, extra[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='failed',released_at='2026-09-30T00:00:00Z',owner_integration_json='{"commit":"abc","baseCommit":"def","changedFiles":[],"at":"2026-09-30T00:00:00Z"}' WHERE id=?`, extra[1].ID); err != nil {
		t.Fatal(err)
	}
	history = append(history, extra[0])
	running := api.NewID("tqe")
	if _, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,created_at,updated_at) VALUES(?,?,?,?,?,'planned',123,'running',2,'mini','/tmp',?,?)`, running, task.ID, items[0].ID, items[0].Revision, orders[0].Seq, ts(s.now()), ts(s.now())); err != nil {
		t.Fatal(err)
	}
	active := []string{extra[1].ID, running}
	page := func(after int64) api.TeamQueueList {
		t.Helper()
		list, err := s.TeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{Limit: 50, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for i, id := range active {
			if list.Entries[i].ID != id || list.Entries[i].Summary {
				t.Fatalf("active entry %d is %s summary %v", i, list.Entries[i].ID, list.Entries[i].Summary)
			}
		}
		return list
	}
	seen := map[string]int{}
	first := page(0)
	if first.History.Total != 121 || first.History.NextAfter != first.Entries[len(first.Entries)-1].Position || len(first.Entries) != 2+50 {
		t.Fatalf("page 1 %+v with %d entries", first.History, len(first.Entries))
	}
	// Finish the running entry between pages: it joins history at a
	// position above the cursor, so older pages neither repeat nor skip.
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='finished' WHERE id=?`, running); err != nil {
		t.Fatal(err)
	}
	active = active[:1]
	pages := []api.TeamQueueList{first}
	for after := first.History.NextAfter; after != 0; {
		next := page(after)
		pages = append(pages, next)
		after = next.History.NextAfter
	}
	if len(pages) != 3 || len(pages[1].Entries) != 1+50 || len(pages[2].Entries) != 1+21 || pages[2].History.NextAfter != 0 || pages[1].History.Total != 122 {
		t.Fatalf("%d pages, sizes %d %d", len(pages), len(pages[1].Entries), len(pages[len(pages)-1].Entries))
	}
	for p, list := range pages {
		last := int64(1 << 62)
		for _, e := range list.Entries {
			if !e.Summary {
				continue
			}
			if e.Position >= last {
				t.Fatalf("page %d not newest first at %d", p, e.Position)
			}
			last = e.Position
			seen[e.ID]++
		}
	}
	for _, h := range history {
		if seen[h.ID] != 1 {
			t.Fatalf("history entry %s at %d listed %d times", h.ID, h.Position, seen[h.ID])
		}
	}
	if len(seen) != len(history) {
		t.Fatalf("listed %d history entries, want %d", len(seen), len(history))
	}
	// Active only, and one item in any state and in full.
	activeList, err := s.TeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil || len(activeList.Entries) != 1 || activeList.Entries[0].ID != extra[1].ID || activeList.History != nil {
		t.Fatalf("active view %+v %v", activeList, err)
	}
	one, err := s.TeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{Item: history[5].ItemID})
	if err != nil || len(one.Entries) != 1 || one.Entries[0].ID != history[5].ID || one.Entries[0].Summary || string(one.Entries[0].LaunchJSON) != `{}` || one.History != nil {
		t.Fatalf("item listing %+v %v", one, err)
	}
	if none, err := s.TeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{Item: items[1].ID}); err != nil || len(none.Entries) != 0 {
		t.Fatalf("item with no entry %+v %v", none, err)
	}
	for _, bad := range []api.TeamQueueListOptions{{View: "all"}, {Item: "tqe_x"}, {Limit: -1}, {Limit: api.MaxLimit + 1}, {After: -1}} {
		if _, err := s.TeamQueuePage(ctx, task.ID, bad); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("options %+v: %v", bad, err)
		}
	}
}

// Block reasons, blockers and stalls read only active entries, so the paged
// listing reports them exactly as the full listing does.
func TestTeamQueuePageMatchesFullListingBlockers(t *testing.T) {
	s, task, items, orders := sharedCheckoutFixture(t, 4)
	ctx := context.Background()
	s.queueStallGrace = time.Nanosecond
	a := addCheckoutEntry(t, s, task, items[0], orders[0], "/main", "src/a")
	addCheckoutEntry(t, s, task, items[1], orders[1], "/main", "src/b")
	addCheckoutEntry(t, s, task, items[2], orders[2], "/other", "src/a/child")
	addCheckoutEntry(t, s, task, items[3], orders[3], "/main", "src/a/other")
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-a", Operation: "claim", EntryID: a.ID, ExpectedRevision: a.Revision, Host: "mini"}); err != nil {
		t.Fatal(err)
	}
	insertHistoryEntries(t, s, task, 100, 5, paddedLaunch(1024, "Implementation"), nil)
	time.Sleep(time.Millisecond)
	full, err := s.ListTeamQueue(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	paged, err := s.TeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stalled := 0
	byID := map[string]api.TeamQueueEntry{}
	for _, e := range paged.Entries {
		byID[e.ID] = e
	}
	for _, want := range full.Entries {
		got, ok := byID[want.ID]
		if !ok {
			t.Fatalf("entry %s missing from the paged listing", want.ID)
		}
		if queueEntryIsHistory(want) {
			continue
		}
		wantStall, _ := json.Marshal(want.Stall)
		gotStall, _ := json.Marshal(got.Stall)
		if got.BlockReason != want.BlockReason || strings.Join(got.BlockedBy, ",") != strings.Join(want.BlockedBy, ",") || string(gotStall) != string(wantStall) {
			t.Fatalf("entry %s: paged %q %v %s, full %q %v %s", want.ID, got.BlockReason, got.BlockedBy, gotStall, want.BlockReason, want.BlockedBy, wantStall)
		}
		if want.Stall != nil {
			stalled++
		}
	}
	if stalled == 0 {
		t.Fatal("the fixture has no stall to compare")
	}
}

// wi_a3ca8b64d12365c2 a5: a launching entry with no live run is its own
// nothing-running stall once the grace has passed, so the list shows it with
// nothing queued behind it. A live member, a paused project or a running
// entry is not.
func TestQueueStallLaunchingEntryWithNothingRunning(t *testing.T) {
	f := newChoresQueue(t, 3, 3, 0)
	advance := f.clock(t)
	a, err := claimEntry(f.s, f.task, f.add(t, 0, "src/a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := claimEntry(f.s, f.task, f.add(t, 1, "src/b"))
	if err != nil {
		t.Fatal(err)
	}
	c := f.run(t, f.add(t, 2, "docs"))
	assertNoStall(t, f, a.ID, "launch inside the grace")
	advance(6 * time.Minute)
	for _, q := range []api.TeamQueueEntry{a, b} {
		assertStall(t, f, q.ID, api.StallNothingRunning, q.ID, "tt team queue fail --task "+f.task.ID+" --entry "+q.ID)
		if got := listedEntry(t, f.s, f.task.ID, q.ID); got.State != "launching" || got.Stall.BlockerRevision != q.Revision || !strings.HasPrefix(got.BlockReason, "Stalled: "+q.ID+" ("+q.ItemID+"): ") {
			t.Fatalf("launch self-stall %+v %+v %q", got.State, got.Stall, got.BlockReason)
		}
	}
	assertNoStall(t, f, c.ID, "a running entry is only a blocker for queued work")
	f.member(t, 0, "lead-a")
	assertNoStall(t, f, a.ID, "a live member")
	assertStall(t, f, b.ID, api.StallNothingRunning, b.ID)
	if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state=? WHERE id=?`, api.ProjectPausePaused, f.task.ID); err != nil {
		t.Fatal(err)
	}
	assertNoStall(t, f, b.ID, "a paused project")
}

// wi_a3ca8b64d12365c2 a6: a launch self-stall posts one Board notice with the
// launch subject and the runner's last error, one line and capped. A retry
// with other error text and a queued entry behind the same launch replay it.
func TestQueueStallLaunchNoticeOnce(t *testing.T) {
	f := newChoresQueue(t, 2, 2, 0)
	ctx := context.Background()
	advance := f.clock(t)
	a, err := claimEntry(f.s, f.task, f.add(t, 0, "src"))
	if err != nil {
		t.Fatal(err)
	}
	b := f.add(t, 1, "src/b")
	advance(6 * time.Minute)
	stall := f.stall(t, a.ID)
	if stall == nil || stall.BlockerEntryID != a.ID {
		t.Fatalf("launch stall %+v", stall)
	}
	id := stall.NoticeRequestID(a.ID)
	if behind := f.stall(t, b.ID); behind == nil || behind.NoticeRequestID(b.ID) != id {
		t.Fatalf("queued entry behind the launch %+v", behind)
	}
	messages := func() []api.Message {
		t.Helper()
		list, err := f.s.ListMessages(ctx, f.task.ID, 0, "", 1000)
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	before := len(messages())
	notice := func(q api.TeamQueueEntry, failure string) error {
		_, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: id, Operation: "stall_notice", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: failure})
		return err
	}
	failure := "freeze: hub: 503 service\x1b[0m\nunavailable\t" + strings.Repeat("é", 600)
	if err := notice(a, failure); err != nil {
		t.Fatal(err)
	}
	all := messages()
	if len(all) != before+1 {
		t.Fatalf("notice posted %d messages", len(all)-before)
	}
	posted := all[len(all)-1]
	env := posted.Envelope
	if env == nil || env.Subject != "A team queue launch has made no progress" || env.Refs["entry"] != a.ID || env.Refs["item"] != a.ItemID || env.Refs["cause"] != api.StallNothingRunning || env.Refs["blocker"] != a.ID || env.Refs["escalation"] != "" {
		t.Fatalf("launch notice %+v", env)
	}
	text := env.Body.Text
	const marker = ". Last launch error on the runner: "
	at := strings.Index(text, marker)
	if !strings.HasPrefix(text, "Stalled: "+a.ID) || at < 0 || !strings.HasSuffix(text, ".") {
		t.Fatalf("launch notice text %q", text)
	}
	detail := strings.TrimSuffix(text[at+len(marker):], ".")
	if !strings.HasPrefix(detail, "freeze: hub: 503 service [0m unavailable é") || strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f }) || len([]rune(detail)) != 500 {
		t.Fatalf("detail %d runes %q", len([]rune(detail)), detail)
	}
	if err := notice(a, "attempt: hub: 502 bad gateway"); err != nil {
		t.Fatalf("retry with other text: %v", err)
	}
	if err := notice(b, ""); err != nil {
		t.Fatalf("queued entry behind the launch: %v", err)
	}
	if n := len(messages()); n != before+1 {
		t.Fatalf("replays posted %d more messages", n-before-1)
	}
}

// wi_a3ca8b64d12365c2 a5, review b1: a relaunched item whose earlier team
// closed long ago is dated from its own claim, so it is neither stalled nor
// announced inside the grace, and is both once past it.
func TestQueueStallLaunchingEntryAfterClosedTeam(t *testing.T) {
	f := newChoresQueue(t, 1, 1, 0)
	ctx := context.Background()
	advance := f.clock(t)
	old := f.member(t, 0, "old-lead")
	if _, err := f.s.db.Exec(`UPDATE agents SET status='closed', last_event_at=? WHERE id=?`, ts(f.s.now()), old.ID); err != nil {
		t.Fatal(err)
	}
	advance(2 * time.Hour)
	q, err := claimEntry(f.s, f.task, f.add(t, 0, "src"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoStall(t, f, q.ID, "a relaunch just claimed")
	id := api.TeamQueueStall{Cause: api.StallNothingRunning, BlockerEntryID: q.ID, BlockerRevision: q.Revision}.NoticeRequestID(q.ID)
	notice := func() error {
		_, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: id, Operation: "stall_notice", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "freeze: hub: 503 unavailable"})
		return err
	}
	messages := func() int {
		t.Helper()
		list, err := f.s.ListMessages(ctx, f.task.ID, 0, "", 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	before := messages()
	if err := notice(); !errors.Is(err, api.ErrConflict) || messages() != before {
		t.Fatalf("notice inside the grace: %v", err)
	}
	advance(4 * time.Minute)
	assertNoStall(t, f, q.ID, "a relaunch inside the grace")
	advance(2 * time.Minute)
	assertStall(t, f, q.ID, api.StallNothingRunning, q.ID)
	if got := f.stall(t, q.ID); got.Since != q.UpdatedAt {
		t.Fatalf("relaunch stall since %s, want the claim %s", got.Since, q.UpdatedAt)
	}
	if err := notice(); err != nil || messages() != before+1 {
		t.Fatalf("notice past the grace: %v, %d messages", err, messages()-before)
	}
}

// smallBug adds a bug with a bounded order to a queue fixture.
func smallBug(t *testing.T, s *Store, task api.Task) (api.WorkItem, api.Message) {
	t.Helper()
	item, err := s.CreateWorkItem(context.Background(), task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Small bug", Priority: "normal", RequestID: api.NewID("req")}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return item, contextLinkedMessage(t, s, task, item, "bounded order", api.NewID("req"), nil)
}

// wi_f8d48780626165cc a1: the queue admits an explicitly chosen small bug and
// keeps Planned as the default.
func TestTeamQueueSmallAdmitsBoundedBug(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	for n := 1; n <= smallChangeMaxOwned; n++ {
		bug, order := smallBug(t, s, task)
		owns := []string{"src/fix.go", "src/fix_test.go", "docs/fix.md"}[:n]
		q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("small-%d", n), Operation: "add", ItemID: bug.ID, OrderMessageSeq: order.Seq, Template: "small", Host: "mini", Cwd: "/tmp", Ownership: owns})
		if err != nil {
			t.Fatalf("%d paths: %v", n, err)
		}
		saved, err := s.GetTeamQueueEntry(ctx, task.ID, q.ID)
		if err != nil || saved.Template != "small" || len(saved.Ownership) != n || saved.Serial {
			t.Fatalf("saved small entry %+v %v", saved, err)
		}
	}
	for i, template := range []string{"", "planned"} {
		q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("planned-%d", i), Operation: "add", ItemID: items[i].ID, OrderMessageSeq: orders[i].Seq, Template: template, Host: "mini", Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		if saved, err := s.GetTeamQueueEntry(ctx, task.ID, q.ID); err != nil || saved.Template != "planned" {
			t.Fatalf("template %q saved %+v %v", template, saved, err)
		}
	}
}

// wi_f8d48780626165cc a2: eligibility is explicit and each refusal names its
// rule; a small entry may narrow but never widen past the cap.
func TestTeamQueueSmallRefusesIneligibleEntries(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	bug, order := smallBug(t, s, task)
	add := func(key string, item api.WorkItem, order api.Message, template string, serial bool, owns ...string) (api.TeamQueueEntry, error) {
		return s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Template: template, Host: "mini", Cwd: "/tmp", Serial: serial, Ownership: owns})
	}
	for _, c := range []struct {
		name, template string
		item           api.WorkItem
		order          api.Message
		serial         bool
		owns           []string
		want           error
		rule           string
	}{
		{"feature", "small", items[0], orders[0], false, []string{"src/a.go"}, api.ErrConflict, "admits only bugs; queue a feature as Planned delivery"},
		{"serial", "small", bug, order, true, nil, api.ErrConflict, "needs explicit ownership (--owns), not --serial"},
		{"unowned", "small", bug, order, false, nil, api.ErrConflict, "needs explicit ownership (--owns), not --serial"},
		{"four paths", "small", bug, order, false, []string{"a", "b", "c", "d"}, api.ErrConflict, "owns at most 3 paths; requeue the item as Planned delivery"},
		{"solo", "solo", bug, order, false, []string{"a"}, api.ErrInvalid, `unknown queue template "solo"; use planned or small`},
		{"pair", "pair", bug, order, false, []string{"a"}, api.ErrInvalid, `unknown queue template "pair"; use planned or small`},
	} {
		_, err := add("refuse-"+strings.ReplaceAll(c.name, " ", "-"), c.item, c.order, c.template, c.serial, c.owns...)
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.rule) {
			t.Fatalf("%s: got %v, want %v naming %q", c.name, err, c.want, c.rule)
		}
	}
	q, err := add("small", bug, order, "small", false, "src/fix", "src/fix_test.go")
	if err != nil {
		t.Fatal(err)
	}
	scope := func(key string, owns ...string) (api.TeamQueueEntry, error) {
		return s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "scope", EntryID: q.ID, ExpectedRevision: q.Revision, Ownership: owns})
	}
	if _, err := scope("widen", "src/fix", "src/fix_test.go", "docs/a.md", "docs/b.md"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "owns at most 3 paths; requeue the item as Planned delivery") {
		t.Fatalf("widening past the cap: %v", err)
	}
	if saved, err := s.GetTeamQueueEntry(ctx, task.ID, q.ID); err != nil || saved.Revision != q.Revision || strings.Join(saved.Ownership, ",") != "src/fix,src/fix_test.go" {
		t.Fatalf("refused widening changed the entry: %+v %v", saved, err)
	}
	// Splitting an owned directory into the files it changed stays a narrowing.
	split, err := scope("split", "src/fix/a.go", "src/fix/b.go", "src/fix/c.go", "src/fix_test.go")
	if err != nil || len(split.Ownership) != 4 {
		t.Fatalf("split narrowing: %+v %v", split, err)
	}
	q = split
	narrowed, err := scope("narrow", "src/fix/a.go")
	if err != nil || strings.Join(narrowed.Ownership, ",") != "src/fix/a.go" || narrowed.Template != "small" {
		t.Fatalf("narrowing: %+v %v", narrowed, err)
	}
}

// wi_f8d48780626165cc a3: the small team's lead is its distinct verifier; the
// store requires independence from builder and reviewers, not from the lead.
func TestTeamQueueSmallLeadMayVerifyReviewerMayNot(t *testing.T) {
	f, h, p := verificationFixture(t)
	admitted := func(name string) api.Agent {
		t.Helper()
		bundle := preparedContextFromAcceptedHistory(t, f.s, f.item, api.MessageReference{TaskID: f.task.ID, Seq: p.OrderMessageSeq})
		a, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "fixture", Session: name, WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: 1, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: p.OrderMessageSeq}, ContextBundle: bundle, TeamRole: api.TeamRoleMember}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead, reviewer := admitted("lead-small"), admitted("reviewer-small")
	if _, err := f.s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, f.task.ID, f.item.ID, lead.ID, lead.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.post(api.Envelope{Kind: "review", Subject: "Review exact fixture candidate", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}, reviewer.ID, 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	byReviewer := p
	byReviewer.OperationKey, byReviewer.VerifierAgentID, byReviewer.VerifierRunID = "reviewer-verifies", reviewer.ID, reviewer.RunID
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "reviewer-plan", AgentID: h.ID, RunID: h.RunID, Plan: &byReviewer}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "distinct from reviewer") {
		t.Fatalf("reviewer verified: %v", err)
	}
	byLead := p
	byLead.OperationKey, byLead.VerifierAgentID, byLead.VerifierRunID = "lead-verifies", lead.ID, lead.RunID
	saveFixtureVerification(t, f, h, byLead, 0)
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := verificationReady(f.ctx, tx, f.item, byLead.Commit); err != nil {
		t.Fatalf("lead receipt not ready: %v", err)
	}
}

// legacyTeamQueueEntriesSQL is the table as hubs created it before an item
// could have more than one entry: the inline one-entry-per-item constraint,
// then the columns later versions added one by one.
const legacyTeamQueueEntriesSQL = `CREATE TABLE team_queue_entries (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), item_id TEXT NOT NULL REFERENCES work_items(id),
 item_revision INTEGER NOT NULL, order_seq INTEGER NOT NULL, template TEXT NOT NULL,
 position INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('queued','launching','running','finished','failed')),
 revision INTEGER NOT NULL DEFAULT 1, host TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '',
 pause_generation INTEGER NOT NULL DEFAULT 0, launch_json BLOB NOT NULL DEFAULT '', close_json BLOB NOT NULL DEFAULT '',
 failure TEXT NOT NULL DEFAULT '', escalation_seq INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(task_id,item_id));
 ALTER TABLE team_queue_entries ADD COLUMN released_at TEXT NOT NULL DEFAULT '';
 ALTER TABLE team_queue_entries ADD COLUMN repository TEXT NOT NULL DEFAULT '';
 ALTER TABLE team_queue_entries ADD COLUMN ownership_json TEXT NOT NULL DEFAULT '[]';
 ALTER TABLE team_queue_entries ADD COLUMN handler_id TEXT NOT NULL DEFAULT '';
 ALTER TABLE team_queue_entries ADD COLUMN handler_run_id TEXT NOT NULL DEFAULT '';
 ALTER TABLE team_queue_entries ADD COLUMN handler_lease_generation INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE team_queue_entries ADD COLUMN base_commit TEXT NOT NULL DEFAULT '';
 ALTER TABLE team_queue_entries ADD COLUMN integration_json TEXT NOT NULL DEFAULT '';
 ALTER TABLE team_queue_entries ADD COLUMN acceptance_json TEXT NOT NULL DEFAULT '';
 ALTER TABLE team_queue_entries ADD COLUMN serial INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE team_queue_entries ADD COLUMN owner_integration_json TEXT NOT NULL DEFAULT '';
 CREATE INDEX team_queue_order ON team_queue_entries(task_id,position);
 CREATE INDEX team_queue_active ON team_queue_entries(task_id,state) WHERE state IN ('launching','running');`

// legacyTeamQueueColumns are the columns of that table, in its order.
const legacyTeamQueueColumns = `id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,close_json,failure,escalation_seq,created_at,updated_at,released_at,repository,ownership_json,handler_id,handler_run_id,handler_lease_generation,base_commit,integration_json,acceptance_json,serial,owner_integration_json`

// dumpTeamQueueRows reads the named columns of every entry as text, by id.
func dumpTeamQueueRows(t *testing.T, s *Store, columns string) map[string][]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT ` + columns + ` FROM team_queue_entries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for rows.Next() {
		values := make([]any, len(names))
		for i := range values {
			values[i] = new([]byte)
		}
		if err := rows.Scan(values...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(names))
		for i := range values {
			row[i] = string(*values[i].(*[]byte))
		}
		out[row[0]] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// a9: an older database keeps every entry row and column through the rebuild
// that drops the one-entry-per-item constraint; a second open changes nothing.
func TestTeamQueueMigrationDropsItemUniqueness(t *testing.T) {
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Migration fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE team_queue_entries`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(legacyTeamQueueEntriesSQL); err != nil {
		t.Fatal(err)
	}
	launch := `{"members":[{"state":"started","runId":"run_1"}]}`
	acceptance := `{"repository":"/repo/.git","commit":"` + strings.Repeat("a", 40) + `","itemRevision":3}`
	var ids, items []string
	for i, state := range []string{"queued", "finished", "failed"} {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Migrated " + state, RequestID: "item-" + state}, by)
		if err != nil {
			t.Fatal(err)
		}
		id := api.NewID("tqe")
		released := ""
		if state == "failed" {
			released = "2026-09-29T10:00:00Z"
		}
		if _, err := s.db.Exec(`INSERT INTO team_queue_entries(`+legacyTeamQueueColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, task.ID, item.ID, 3+i, 40+i, "planned", i+1, state, 7+i, "mini", "/work/"+state, 2, []byte(launch), []byte(`{"requestId":"close"}`), "cause "+state, 90+i, "2026-09-28T10:00:00Z", "2026-09-29T09:00:00Z",
			released, "/repo/.git", `["hub/a.go"]`, "agt_handler", "run_handler", 4+i, strings.Repeat("b", 40), `{"commit":"c"}`, acceptance, i%2, `{"commit":"d"}`); err != nil {
			t.Fatal(err)
		}
		ids, items = append(ids, id), append(items, item.ID)
	}
	before := dumpTeamQueueRows(t, s, legacyTeamQueueColumns)
	if len(before) != 3 {
		t.Fatalf("fixture rows %d", len(before))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	schema := func(s *Store) string {
		t.Helper()
		rows, err := s.db.Query(`SELECT type,name,COALESCE(sql,''),rootpage FROM sqlite_master WHERE tbl_name='team_queue_entries' ORDER BY type,name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var kind, name, sqlText string
			var root int
			if err := rows.Scan(&kind, &name, &sqlText, &root); err != nil {
				t.Fatal(err)
			}
			if kind != "table" {
				// Only the table's page shows whether it was rebuilt again.
				root = 0
			}
			out = append(out, fmt.Sprintf("%s %s %d %s", kind, name, root, sqlText))
		}
		return strings.Join(out, "\n")
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if after := dumpTeamQueueRows(t, s, legacyTeamQueueColumns); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("rebuild changed rows\nbefore %v\nafter  %v", before, after)
	}
	for id, row := range dumpTeamQueueRows(t, s, `id,attempt,retry_of`) {
		if row[1] != "1" || row[2] != "" {
			t.Fatalf("entry %s attempt %q retry_of %q", id, row[1], row[2])
		}
	}
	first := schema(s)
	var tableSQL string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='team_queue_entries'`).Scan(&tableSQL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(strings.Fields(tableSQL), ""), "UNIQUE(task_id,item_id)") {
		t.Fatalf("inline uniqueness survived: %s", tableSQL)
	}
	for _, index := range []string{"team_queue_order", "team_queue_active", "team_queue_item_attempt", "team_queue_item_live"} {
		if !strings.Contains(first, "index "+index+" ") {
			t.Fatalf("index %s is missing:\n%s", index, first)
		}
	}
	var leftover int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='team_queue_entries_v2'`).Scan(&leftover); err != nil || leftover != 0 {
		t.Fatalf("rebuild left its staging table: %d %v", leftover, err)
	}
	// The migrated rows read through the store, and the indexes now carry the
	// rules: attempts of one item are distinct and at most one is live.
	finished, err := s.GetTeamQueueEntry(ctx, task.ID, ids[1])
	if err != nil || finished.State != "finished" || finished.Attempt != 1 || finished.Acceptance == nil || finished.Acceptance.ItemRevision != 3 || finished.HandlerLeaseGeneration != 5 || !finished.Serial {
		t.Fatalf("migrated entry %+v %v", finished, err)
	}
	insert := func(item, state string, attempt int) error {
		_, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,attempt,created_at,updated_at) VALUES(?,?,?,1,1,'planned',9,?,?,'now','now')`, api.NewID("tqe"), task.ID, item, state, attempt)
		return err
	}
	if err := insert(items[2], "queued", 1); err == nil {
		t.Fatal("a second attempt 1 was stored")
	}
	if err := insert(items[0], "queued", 2); err == nil {
		t.Fatal("a second live entry was stored")
	}
	if err := insert(items[2], "queued", 2); err != nil {
		t.Fatalf("a retry of a released failed entry was refused: %v", err)
	}
	rows := dumpTeamQueueRows(t, s, legacyTeamQueueColumns+`,attempt,retry_of`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if second := schema(s); second != first {
		t.Fatalf("second open changed the schema\nfirst  %s\nsecond %s", first, second)
	}
	if again := dumpTeamQueueRows(t, s, legacyTeamQueueColumns+`,attempt,retry_of`); fmt.Sprint(again) != fmt.Sprint(rows) {
		t.Fatalf("second open changed rows\nbefore %v\nafter  %v", rows, again)
	}
}

// rebindFixture is one item with a confirmed order and a repository-backed
// queue entry: queued, or running with a lead, a worker and a closed member
// admitted through the real binding path.
type rebindFixture struct {
	s                  *Store
	ctx                context.Context
	by                 api.Caller
	task               api.Task
	item               api.WorkItem
	order              api.Message
	handler            api.Agent
	entry              api.TeamQueueEntry
	lead, worker, gone api.Agent
}

const rebindRepository = "/fixture/repo/.git"

func newRebindFixture(t *testing.T, running bool) *rebindFixture {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	f := &rebindFixture{s: s, ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}, task: task, item: items[0], order: orders[0]}
	agents, err := s.ListAgents(f.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler {
			f.handler = a
		}
	}
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order.Seq, Host: "mini", Cwd: "/worktrees/rebind", Repository: rebindRepository, BaseCommit: strings.Repeat("a", 40), Ownership: []string{"hub/internal/store"}})
	if !running {
		return f
	}
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "claim", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Host: "mini"})
	f.lead, f.worker, f.gone = f.member(t, "lead"), f.member(t, "worker"), f.member(t, "gone")
	if _, err := s.db.Exec(`UPDATE agents SET status='closed',cleanup_done=1 WHERE id=?`, f.gone.ID); err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(map[string]any{"task": task.ID, "item": f.item.ID, "revision": f.item.Revision, "order": f.order.Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": f.lead.RunID, "fields": map[string]any{"agentId": f.lead.ID, "name": f.lead.Name, "cwd": "/worktrees/rebind"}}}})
	for _, step := range []api.TeamQueueRequest{{Operation: "freeze", LaunchJSON: plan}, {Operation: "attempt"}, {Operation: "started", MemberRunID: f.lead.RunID}, {Operation: "running"}} {
		step.EntryID, step.ExpectedRevision = f.entry.ID, f.entry.Revision
		f.entry = f.action(t, step)
	}
	return f
}

// action runs one queue operation that must succeed.
func (f *rebindFixture) action(t *testing.T, req api.TeamQueueRequest) api.TeamQueueEntry {
	t.Helper()
	if req.RequestID == "" {
		req.RequestID = api.NewID("req")
	}
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Operation, err)
	}
	return q
}

// member admits a live agent bound to the item at its current revision.
func (f *rebindFixture) member(t *testing.T, name string) api.Agent {
	t.Helper()
	item, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref := api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}
	a, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: name, AgentID: api.NewID("agt"), Host: "mini", Session: name,
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: ref, ContextBundle: syntheticPreparedContext(t, item, ref, syntheticHistory(item, f.order))}}, f.by)
	if err != nil {
		t.Fatalf("admit %s: %v", name, err)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(f.s.now()), a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

// amend saves a scope amendment and posts its message; the item moves on.
func (f *rebindFixture) amend(t *testing.T, key string) (api.WorkItem, api.Message) {
	t.Helper()
	current, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	description := "amended scope " + key
	updated, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: current.Revision, Description: &description}, api.Caller{Node: "fixture", User: "amender"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision == current.Revision {
		t.Fatalf("amendment kept revision %d", updated.Revision)
	}
	source, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "scope amendment " + key, RequestID: "amend-" + key, WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: updated.Revision, Relationship: "primary"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return updated, source
}

// confirm files the handler's scope confirmation of the entry's order at the
// item's amended revision, which a rebind requires.
func (f *rebindFixture) confirm(t *testing.T, item api.WorkItem) {
	t.Helper()
	if _, err := f.s.ConfirmWorkOrderScope(f.ctx, f.task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: api.NewID("req"), AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: f.order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
}

// rebind is the leased handler's rebind request to the given revision.
func (f *rebindFixture) rebind(item api.WorkItem, source api.Message) api.TeamQueueRequest {
	return api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "rebind", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, ItemRevision: item.Revision, SourceMessageSeq: source.Seq,
		HandlerAgentID: f.handler.ID, HandlerRunID: f.handler.RunID, Caller: api.Caller{Node: "handler-node", User: "approver"}}
}

// bindings reads the item's binding rows as text, by agent.
func (f *rebindFixture) bindings(t *testing.T) map[string][]string {
	t.Helper()
	rows, err := f.s.db.Query(`SELECT agent_id,run_id,item_revision,context_digest,created_at,context_through_message_seq,hex(context_json) FROM agent_work_item_bindings WHERE item_task_id=? AND item_id=?`, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		row := make([]string, 7)
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3], &row[4], &row[5], &row[6]); err != nil {
			t.Fatal(err)
		}
		out[row[0]] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// a1: an amendment after queueing no longer strands a queued entry. The
// rebind moves it to the new revision under the same identity, keeps who
// amended and who approved, and the entry then claims.
func TestTeamQueueRebindQueuedEntry(t *testing.T) {
	f := newRebindFixture(t, false)
	updated, source := f.amend(t, "one")
	f.confirm(t, updated)
	req := f.rebind(updated, source)
	rebound, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.ID != f.entry.ID || rebound.ItemID != f.item.ID || rebound.State != "queued" || rebound.ItemRevision != updated.Revision || rebound.Revision != f.entry.Revision+1 || rebound.Position != f.entry.Position || rebound.OrderMessageSeq != f.order.Seq {
		t.Fatalf("rebound entry %+v", rebound)
	}
	if replay, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req); err != nil || replay.Revision != rebound.Revision || len(replay.Rebinds) != 1 {
		t.Fatalf("rebind replay %+v %v", replay, err)
	}
	stored, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
	if err != nil || stored.ItemRevision != updated.Revision || len(stored.Rebinds) != 1 {
		t.Fatalf("stored entry %+v %v", stored, err)
	}
	history := stored.Rebinds[0]
	want := api.TeamQueueRebind{ID: history.ID, EntryID: f.entry.ID, FromItemRevision: f.item.Revision, ToItemRevision: updated.Revision, FromScopeRevision: f.item.ScopeRevision, ToScopeRevision: updated.ScopeRevision,
		OrderMessageSeq: f.order.Seq, SourceMessageSeq: source.Seq, AmendedBy: api.Sender{Node: "fixture", User: "amender"},
		ApprovedBy: api.Sender{AgentID: f.handler.ID, Node: "handler-node", User: "approver"}, ApprovedRunID: f.handler.RunID, EntryState: "queued", CreatedAt: history.CreatedAt}
	if !reflect.DeepEqual(history, want) || history.ID == "" || history.CreatedAt == "" || updated.ScopeRevision == f.item.ScopeRevision {
		t.Fatalf("rebind history\n got %+v\nwant %+v", history, want)
	}
	if len(rebound.Rebinds) != 1 || !reflect.DeepEqual(rebound.Rebinds[0], history) {
		t.Fatalf("rebind result history %+v", rebound.Rebinds)
	}
	byItem, err := f.s.TeamQueuePage(f.ctx, f.task.ID, api.TeamQueueListOptions{Item: f.item.ID})
	if err != nil || len(byItem.Entries) != 1 || len(byItem.Entries[0].Rebinds) != 1 || byItem.Entries[0].BlockReason != "" {
		t.Fatalf("item listing %+v %v", byItem, err)
	}
	claimed, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "claim-rebound", Operation: "claim", EntryID: rebound.ID, ExpectedRevision: rebound.Revision, Host: "mini"})
	if err != nil || claimed.State != "launching" || claimed.ItemRevision != updated.Revision {
		t.Fatalf("claim after rebind %+v %v", claimed, err)
	}
}

// a2: a running team is no longer stranded. Its entry and live bindings move
// together; each binding keeps its run, digest, stored context and creation
// time; a closed member is left alone; the lead is told once.
func TestTeamQueueRebindRunningEntryMovesBindings(t *testing.T) {
	f := newRebindFixture(t, true)
	before := f.bindings(t)
	updated, source := f.amend(t, "one")
	f.confirm(t, updated)
	rebound, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.rebind(updated, source))
	if err != nil {
		t.Fatal(err)
	}
	if rebound.ID != f.entry.ID || rebound.State != "running" || rebound.ItemRevision != updated.Revision || string(rebound.LaunchJSON) != string(f.entry.LaunchJSON) || rebound.HandlerRunID != f.entry.HandlerRunID {
		t.Fatalf("rebound entry %+v", rebound)
	}
	after := f.bindings(t)
	if len(after) != 3 {
		t.Fatalf("bindings %d", len(after))
	}
	old, moved := fmt.Sprint(f.item.Revision), fmt.Sprint(updated.Revision)
	for _, a := range []api.Agent{f.lead, f.worker} {
		was, now := before[a.ID], after[a.ID]
		if was[2] != old || now[2] != moved {
			t.Fatalf("%s binding revision %s -> %s", a.Name, was[2], now[2])
		}
		was[2] = now[2]
		if !reflect.DeepEqual(was, now) || now[1] != a.RunID || now[3] != a.WorkItem.ContextDigest {
			t.Fatalf("%s binding identity changed\nbefore %v\nafter  %v", a.Name, was, now)
		}
	}
	if !reflect.DeepEqual(before[f.gone.ID], after[f.gone.ID]) || after[f.gone.ID][2] != old {
		t.Fatalf("closed binding changed %v", after[f.gone.ID])
	}
	if len(rebound.Rebinds) != 1 || rebound.Rebinds[0].EntryState != "running" || len(rebound.Rebinds[0].Bindings) != 2 {
		t.Fatalf("rebind history %+v", rebound.Rebinds)
	}
	seen := map[string]bool{}
	for _, b := range rebound.Rebinds[0].Bindings {
		if b.FromItemRevision != f.item.Revision || b.ToItemRevision != updated.Revision || b.RunID != after[b.AgentID][1] || b.ContextDigest != after[b.AgentID][3] {
			t.Fatalf("binding history %+v", b)
		}
		seen[b.AgentID] = true
	}
	if !seen[f.lead.ID] || !seen[f.worker.ID] {
		t.Fatalf("binding history names %v", seen)
	}
	var rows int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_rebind_bindings`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("binding history rows %d %v", rows, err)
	}
	messages, err := f.s.ListMessages(f.ctx, f.task.ID, source.Seq, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range messages {
		if m.Envelope == nil || m.Envelope.Refs["entry"] != f.entry.ID {
			continue
		}
		notices++
		if m.Envelope.Kind != api.EnvelopeKindNotice || m.To != f.lead.ID || len(m.WorkItems) != 1 || m.WorkItems[0].ItemID != f.item.ID || m.WorkItems[0].ItemRevision != updated.Revision || m.Envelope.Refs["fromRevision"] != old || m.Envelope.Refs["toRevision"] != moved {
			t.Fatalf("lead notice %+v", m)
		}
	}
	if notices != 1 {
		t.Fatalf("lead notices %d", notices)
	}
	// The lead's own context still reads at its admitted bundle.
	context, err := f.s.GetAgentWorkItemContext(f.ctx, f.task.ID, f.lead.ID, f.lead.RunID)
	if err != nil || context.Binding.ItemRevision != updated.Revision || context.Binding.ContextDigest != f.lead.WorkItem.ContextDigest {
		t.Fatalf("lead context %+v %v", context.Binding, err)
	}
}

// a5: every case a rebind does not support is refused naming the entry, and
// neither the entry nor a binding changes.
func TestTeamQueueRebindRefusals(t *testing.T) {
	cases := []struct {
		name    string
		running bool
		invalid bool
		want    string
		prepare func(t *testing.T, f *rebindFixture) api.TeamQueueRequest
	}{
		{name: "launching", want: "is launching", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			f.entry = f.action(t, api.TeamQueueRequest{Operation: "claim", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Host: "mini"})
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			return f.rebind(updated, source)
		}},
		{name: "failed", want: "tt team queue requeue", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			f.entry = f.action(t, api.TeamQueueRequest{Operation: "fail", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Failure: "fixture failure"})
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			return f.rebind(updated, source)
		}},
		{name: "finished", want: "is finished", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.entry.ID); err != nil {
				t.Fatal(err)
			}
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			return f.rebind(updated, source)
		}},
		{name: "accepted running", running: true, want: "saved acceptance", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET acceptance_json=? WHERE id=?`, `{"repository":"`+rebindRepository+`","itemRevision":1}`, f.entry.ID); err != nil {
				t.Fatal(err)
			}
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			return f.rebind(updated, source)
		}},
		{name: "terminal item", want: "terminal item", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			updated, source := f.amend(t, "x")
			status := "dismissed"
			dismissed, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: updated.Revision, Status: &status}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			return f.rebind(dismissed, source)
		}},
		{name: "wrong item revision", want: "rebind to the current revision", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			return f.rebind(f.item, source)
		}},
		{name: "not amended", want: "already bound", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			return f.rebind(f.item, f.order)
		}},
		{name: "no scope confirmation", want: "confirm scope for revision", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			updated, source := f.amend(t, "x")
			return f.rebind(updated, source)
		}},
		{name: "unlinked source", want: "is not linked to its item", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			updated, _ := f.amend(t, "x")
			f.confirm(t, updated)
			other, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "unrelated board message", RequestID: "unrelated"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			return f.rebind(updated, other)
		}},
		{name: "lead as caller", running: true, want: "only the owner or a database handler", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			req := f.rebind(updated, source)
			req.HandlerAgentID, req.HandlerRunID, req.LeadAgentID, req.LeadRunID = "", "", f.lead.ID, f.lead.RunID
			return req
		}},
		{name: "non-leased handler on a running entry", running: true, want: "leased handler", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			other, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "database-two", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "database-two"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(f.s.now()), other.ID); err != nil {
				t.Fatal(err)
			}
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			req := f.rebind(updated, source)
			req.HandlerAgentID, req.HandlerRunID = other.ID, other.RunID
			return req
		}},
		{name: "stale handler run", want: "only the owner or a database handler", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			req := f.rebind(updated, source)
			req.HandlerRunID = api.NewID("run")
			return req
		}},
		{name: "no source", invalid: true, want: "--source", prepare: func(t *testing.T, f *rebindFixture) api.TeamQueueRequest {
			updated, source := f.amend(t, "x")
			f.confirm(t, updated)
			req := f.rebind(updated, source)
			req.SourceMessageSeq = 0
			return req
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebindFixture(t, c.running)
			req := c.prepare(t, f)
			req.ExpectedRevision = f.entry.Revision
			entry, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			req.ExpectedRevision = entry.Revision
			bindings := f.bindings(t)
			_, err = f.s.TeamQueueAction(f.ctx, f.task.ID, req)
			sentinel := api.ErrConflict
			if c.invalid {
				sentinel = api.ErrInvalid
			}
			if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), f.entry.ID) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refusal = %v, want %v naming %s with %q", err, sentinel, f.entry.ID, c.want)
			}
			if same, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID); err != nil || !reflect.DeepEqual(same, entry) {
				t.Fatalf("refused rebind changed the entry\nbefore %+v\nafter  %+v %v", entry, same, err)
			}
			if after := f.bindings(t); !reflect.DeepEqual(after, bindings) {
				t.Fatalf("refused rebind changed bindings\nbefore %v\nafter  %v", bindings, after)
			}
			var history int
			if err := f.s.db.QueryRow(`SELECT (SELECT count(*) FROM team_queue_rebinds)+(SELECT count(*) FROM team_queue_rebind_bindings)`).Scan(&history); err != nil || history != 0 {
				t.Fatalf("refused rebind left history rows: %d %v", history, err)
			}
		})
	}
}

// a6: a bound agent keeps posting across a rebind. After the item moves on
// again, its posts linked at the revision it was admitted at and at the
// revision it was rebound to are both accepted; a revision its binding never
// held is still refused.
func TestBoundAgentPostsAcrossRebind(t *testing.T) {
	f := newRebindFixture(t, true)
	first, source := f.amend(t, "one")
	f.confirm(t, first)
	f.entry = f.action(t, f.rebind(first, source))
	skipped, _ := f.amend(t, "two")
	current, _ := f.amend(t, "three")
	post := func(key string, revision int64) error {
		_, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "progress " + key, RequestID: "post-" + key, AgentID: f.worker.ID,
			WorkItems:        []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: revision, Relationship: "primary"}},
			WorkOrderMessage: &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}}, f.by)
		return err
	}
	if err := post("admitted", f.item.Revision); err != nil {
		t.Fatalf("post at the admitted revision %d: %v", f.item.Revision, err)
	}
	if err := post("rebound", first.Revision); err != nil {
		t.Fatalf("post at the rebound revision %d: %v", first.Revision, err)
	}
	if err := post("never-held", skipped.Revision); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("post at revision %d the binding never held = %v", skipped.Revision, err)
	}
	if current.Revision <= skipped.Revision || skipped.Revision <= first.Revision {
		t.Fatalf("fixture revisions %d %d %d", first.Revision, skipped.Revision, current.Revision)
	}
	// The closed member was not rebound: only its admitted revision is held.
	if _, err := f.s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, f.gone.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "stale member", RequestID: "post-unmoved", AgentID: f.gone.ID,
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: first.Revision, Relationship: "primary"}},
		WorkOrderMessage: &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}}, f.by)
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("unmoved binding posted at the rebound revision: %v", err)
	}
}

// a4: after a running rebind the same entry and item go on to handler
// acceptance, team close at the rebound revision and finish.
func TestTeamQueueRebindThenAcceptAndFinish(t *testing.T) {
	f := newRebindFixture(t, true)
	updated, source := f.amend(t, "one")
	f.confirm(t, updated)
	f.entry = f.action(t, f.rebind(updated, source))

	seedPassingVerificationAt(t, f.s, updated, candidateB, autoWorktree)
	report, _, err := f.s.PutNarrativeReport(f.ctx, f.task.ID, f.item.ID, completeReportRequest(updated, "rebound-report", 5), f.by)
	if err != nil {
		t.Fatal(err)
	}
	done := "done"
	if _, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: updated.Revision, Status: &done, AgentID: f.handler.ID, RunID: f.handler.RunID, RequestID: "done-save",
		CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision},
		QueueAcceptance:  &api.WorkItemQueueAcceptance{EntryID: f.entry.ID, Worktree: autoWorktree, Branch: autoBranch, Commit: candidateB}}, f.by); err != nil {
		t.Fatalf("done save with acceptance: %v", err)
	}
	item, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID)
	if err != nil || item.ID != f.item.ID || item.Status != "done" {
		t.Fatalf("item %+v %v", item, err)
	}
	accepted, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
	if err != nil || accepted.State != "running" || accepted.Acceptance == nil || accepted.Acceptance.Commit != candidateB || accepted.Acceptance.ItemRevision != item.Revision || accepted.ItemRevision != updated.Revision || len(accepted.Rebinds) != 1 {
		t.Fatalf("accepted entry %+v %v", accepted, err)
	}

	closeReq := api.TeamCloseRequest{RequestID: "close-rebound", ActorAgentID: f.lead.ID, ActorRunID: f.lead.RunID, LeadAgentID: f.lead.ID, LeadRunID: f.lead.RunID, LeadRevision: 1, ItemID: f.item.ID, ItemRevision: accepted.ItemRevision}
	for _, a := range []api.Agent{f.lead, f.worker} {
		live, err := f.s.GetAgent(f.ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		closeReq.Members = append(closeReq.Members, api.TeamCloseMember{AgentID: live.ID, RunID: live.RunID, Host: live.Host, Status: live.Status})
	}
	if closeReq.Members[0].AgentID > closeReq.Members[1].AgentID {
		closeReq.Members[0], closeReq.Members[1] = closeReq.Members[1], closeReq.Members[0]
	}
	stale := closeReq
	stale.RequestID, stale.ItemRevision = "close-old-revision", f.item.Revision
	if _, err := f.s.CloseItemTeam(f.ctx, f.task.ID, stale, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("team close at the pre-rebind revision = %v", err)
	}
	closed, err := f.s.CloseItemTeam(f.ctx, f.task.ID, closeReq, f.by)
	if err != nil || closed.ItemID != f.item.ID || closed.LeadAgentID != f.lead.ID || len(closed.Members) != 2 {
		t.Fatalf("team close at the rebound revision %+v %v", closed, err)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET cleanup_done=1 WHERE id IN (?,?)`, f.lead.ID, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	closeJSON, _ := json.Marshal(closeReq)
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "close", EntryID: f.entry.ID, ExpectedRevision: accepted.Revision, CloseJSON: closeJSON})
	a := accepted.Acceptance
	finished := f.action(t, api.TeamQueueRequest{Operation: "finish", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision,
		Integration: &api.TeamIntegrationReady{Repository: a.Repository, BaseCommit: a.BaseCommit, Worktree: a.Worktree, Branch: a.Branch, Commit: a.Commit, Evidence: a.Evidence}})
	if finished.ID != accepted.ID || finished.ItemID != f.item.ID || finished.State != "finished" || finished.Integration == nil || finished.ItemRevision != updated.Revision || finished.Attempt != 1 {
		t.Fatalf("finished entry %+v", finished)
	}
	var entries int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_entries WHERE task_id=?`, f.task.ID).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("entries %d %v: the rebound entry finished without a replacement", entries, err)
	}
}

// failAndRelease fails the fixture's entry, closes and cleans its team and
// releases it, as the owner or runner would.
func (f *rebindFixture) failAndRelease(t *testing.T) {
	t.Helper()
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "fail", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Failure: "fixture failure"})
	if _, err := f.s.db.Exec(`UPDATE agents SET status='closed',cleanup_done=1 WHERE task_id=? AND role=''`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "release", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision})
}

// requeue is the owner's retry request for the fixture's failed entry.
func (f *rebindFixture) requeue() api.TeamQueueRequest {
	return api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "requeue", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision}
}

// a7: an item whose entry failed is retried as a new attempt under the same
// item, at its current revision, with the failed entry kept as history. The
// new attempt claims, freezes over the released lead row and runs.
func TestTeamQueueRequeueFailedEntryAsNewAttempt(t *testing.T) {
	f := newRebindFixture(t, true)
	f.failAndRelease(t)
	// The owner corrects the scope after the failure; the retry picks it up.
	updated, _ := f.amend(t, "after-failure")
	f.confirm(t, updated)
	failed, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
	if err != nil || failed.State != "failed" || failed.ReleasedAt == "" || failed.Attempt != 1 {
		t.Fatalf("failed entry %+v %v", failed, err)
	}
	req := f.requeue()
	retry, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID == failed.ID || !validTeamQueueID(retry.ID) || retry.ItemID != f.item.ID || retry.Attempt != 2 || retry.RetryOf != failed.ID || retry.State != "queued" || retry.Revision != 1 ||
		retry.ItemRevision != updated.Revision || retry.OrderMessageSeq != failed.OrderMessageSeq || retry.Template != failed.Template || retry.Host != failed.Host || retry.Cwd != failed.Cwd || retry.Repository != failed.Repository ||
		retry.BaseCommit != failed.BaseCommit || !reflect.DeepEqual(retry.Ownership, failed.Ownership) || retry.Position <= failed.Position || len(retry.LaunchJSON) != 0 || retry.HandlerID != "" || retry.Failure != "" {
		t.Fatalf("retry %+v\nof    %+v", retry, failed)
	}
	if replay, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req); err != nil || replay.ID != retry.ID {
		t.Fatalf("requeue replay %+v %v", replay, err)
	}
	if same, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, failed.ID); err != nil || !reflect.DeepEqual(same, failed) {
		t.Fatalf("requeue changed the failed entry\nbefore %+v\nafter  %+v %v", failed, same, err)
	}

	q := f.action(t, api.TeamQueueRequest{Operation: "claim", EntryID: retry.ID, ExpectedRevision: retry.Revision, Host: "mini"})
	lead := f.member(t, "lead-two")
	plan, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": f.item.ID, "revision": updated.Revision, "order": f.order.Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": lead.RunID, "fields": map[string]any{"agentId": lead.ID, "name": lead.Name, "cwd": "/worktrees/rebind"}}}})
	for _, step := range []api.TeamQueueRequest{{Operation: "freeze", LaunchJSON: plan}, {Operation: "attempt"}, {Operation: "started", MemberRunID: lead.RunID}, {Operation: "running"}} {
		step.EntryID, step.ExpectedRevision = q.ID, q.Revision
		q = f.action(t, step)
	}
	if q.State != "running" || q.Attempt != 2 || q.ID != retry.ID {
		t.Fatalf("running retry %+v", q)
	}
	var leadAgent, leadRun, leadState string
	var leadRows, leadRevision int
	if err := f.s.db.QueryRow(`SELECT count(*),agent_id,run_id,state,revision FROM item_team_leads WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID).Scan(&leadRows, &leadAgent, &leadRun, &leadState, &leadRevision); err != nil {
		t.Fatal(err)
	}
	if leadRows != 1 || leadAgent != lead.ID || leadRun != lead.RunID || leadState != "running" || leadRevision != 3 {
		t.Fatalf("lead row count=%d %s/%s %s revision=%d", leadRows, leadAgent, leadRun, leadState, leadRevision)
	}

	byItem, err := f.s.TeamQueuePage(f.ctx, f.task.ID, api.TeamQueueListOptions{Item: f.item.ID})
	if err != nil || len(byItem.Entries) != 2 || byItem.History != nil {
		t.Fatalf("item listing %+v %v", byItem, err)
	}
	newest, oldest := byItem.Entries[0], byItem.Entries[1]
	if newest.ID != retry.ID || newest.Attempt != 2 || newest.Summary || len(newest.LaunchJSON) == 0 || oldest.ID != failed.ID || oldest.Attempt != 1 || oldest.Summary || string(oldest.LaunchJSON) != string(failed.LaunchJSON) || oldest.Failure != failed.Failure {
		t.Fatalf("item listing order\nnewest %+v\noldest %+v", newest, oldest)
	}
	// Both attempts appear in the project's queue: one active, one history.
	list, err := f.s.TeamQueuePage(f.ctx, f.task.ID, api.TeamQueueListOptions{})
	if err != nil || len(list.Entries) != 2 || list.Entries[0].ID != retry.ID || list.Entries[1].ID != failed.ID || !list.Entries[1].Summary || list.Entries[1].Attempt != 1 {
		t.Fatalf("queue listing %+v %v", list, err)
	}
	var items int
	if err := f.s.db.QueryRow(`SELECT count(DISTINCT item_id) FROM team_queue_entries WHERE task_id=?`, f.task.ID).Scan(&items); err != nil || items != 1 {
		t.Fatalf("items with entries %d %v: the retry kept the item identity", items, err)
	}
}

// a8: a requeue is refused, naming the entry and the supported path, unless
// it retries the item's released failed latest attempt with nothing live.
func TestTeamQueueRequeueRefusals(t *testing.T) {
	refused := func(t *testing.T, f *rebindFixture, req api.TeamQueueRequest, id string, want ...string) {
		t.Helper()
		var before int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_entries WHERE task_id=?`, f.task.ID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if req.RequestID == "" {
			req.RequestID = api.NewID("req")
		}
		_, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), id) {
			t.Fatalf("refusal = %v, want a conflict naming %s", err, id)
		}
		for _, text := range want {
			if !strings.Contains(err.Error(), text) {
				t.Fatalf("refusal %q lacks %q", err, text)
			}
		}
		var after int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_entries WHERE task_id=?`, f.task.ID).Scan(&after); err != nil || after != before {
			t.Fatalf("refused requeue changed entries %d -> %d %v", before, after, err)
		}
	}
	t.Run("live entry", func(t *testing.T) {
		f := newRebindFixture(t, false)
		refused(t, f, f.requeue(), f.entry.ID, "is queued", "only a released failed entry")
	})
	t.Run("unreleased failed entry", func(t *testing.T) {
		f := newRebindFixture(t, false)
		f.entry = f.action(t, api.TeamQueueRequest{Operation: "fail", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Failure: "fixture failure"})
		refused(t, f, f.requeue(), f.entry.ID, "not released", "tt team queue release --task "+f.task.ID+" --entry "+f.entry.ID)
		// A plain add is pointed at the same path.
		refused(t, f, api.TeamQueueRequest{Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order.Seq, Host: "mini", Cwd: "/tmp"}, f.entry.ID, "tt team queue requeue")
	})
	t.Run("live item-bound run", func(t *testing.T) {
		f := newRebindFixture(t, true)
		f.failAndRelease(t)
		if _, err := f.s.db.Exec(`UPDATE agents SET status='running',cleanup_done=0 WHERE id=?`, f.worker.ID); err != nil {
			t.Fatal(err)
		}
		refused(t, f, f.requeue(), f.entry.ID, "1 item-bound runs are still live or uncleaned")
	})
	t.Run("terminal item", func(t *testing.T) {
		f := newRebindFixture(t, false)
		f.failAndRelease(t)
		status := "dismissed"
		if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &status}, f.by); err != nil {
			t.Fatal(err)
		}
		refused(t, f, f.requeue(), f.entry.ID, "its item is dismissed")
	})
	t.Run("amended item without confirmed scope", func(t *testing.T) {
		f := newRebindFixture(t, false)
		f.failAndRelease(t)
		f.amend(t, "x")
		refused(t, f, f.requeue(), f.entry.ID, "scope is not confirmed", "tt team queue requeue")
	})
	t.Run("lead as caller", func(t *testing.T) {
		f := newRebindFixture(t, true)
		f.failAndRelease(t)
		req := f.requeue()
		req.LeadAgentID, req.LeadRunID = f.lead.ID, f.lead.RunID
		refused(t, f, req, f.entry.ID, "only by the owner or a database handler")
	})
	t.Run("second requeue and plain add while attempt two is live", func(t *testing.T) {
		f := newRebindFixture(t, false)
		f.failAndRelease(t)
		// A plain add for an item with a failed entry names the retry.
		refused(t, f, api.TeamQueueRequest{Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order.Seq, Host: "mini", Cwd: "/tmp"}, f.entry.ID, "(failed)", "tt team queue requeue --task "+f.task.ID+" --entry "+f.entry.ID)
		req := f.requeue()
		req.HandlerAgentID, req.HandlerRunID = f.handler.ID, f.handler.RunID
		retry := f.action(t, req)
		if retry.Attempt != 2 || retry.RetryOf != f.entry.ID {
			t.Fatalf("handler requeue %+v", retry)
		}
		refused(t, f, f.requeue(), f.entry.ID, "not the item's latest attempt", retry.ID)
		refused(t, f, api.TeamQueueRequest{Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order.Seq, Host: "mini", Cwd: "/tmp"}, retry.ID, "already in team queue")
	})
	t.Run("small attempt", func(t *testing.T) {
		f := newRebindFixture(t, false)
		bug, order := smallBug(t, f.s, f.task)
		f.item, f.order = bug, order
		f.entry = f.action(t, api.TeamQueueRequest{Operation: "add", ItemID: bug.ID, OrderMessageSeq: order.Seq, Template: "small", Host: "mini", Cwd: "/tmp", Ownership: []string{"src/fix.go", "src/fix_test.go"}})
		f.failAndRelease(t)
		over := f.requeue()
		over.Ownership = []string{"src/a.go", "src/b.go", "src/c.go", "src/d.go"}
		refused(t, f, over, f.entry.ID, "owns at most 3 paths", "--template planned")
		// Requeue as Planned: the fix outgrew the lane.
		planned := f.requeue()
		planned.Template, planned.Ownership = "planned", over.Ownership
		retry := f.action(t, planned)
		if retry.Template != "planned" || retry.State != "queued" || retry.Attempt != 2 || retry.RetryOf != f.entry.ID || len(retry.Ownership) != 4 || retry.ItemID != bug.ID {
			t.Fatalf("planned retry of a small entry %+v", retry)
		}
		if failed, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID); err != nil || failed.Template != "small" || len(failed.Ownership) != 2 {
			t.Fatalf("failed small entry %+v %v", failed, err)
		}
	})
	t.Run("small attempt of a feature", func(t *testing.T) {
		f := newRebindFixture(t, false)
		f.failAndRelease(t)
		req := f.requeue()
		req.Template, req.Ownership = "small", []string{"src/fix.go"}
		refused(t, f, req, f.entry.ID, "admits only bugs", "--template planned")
	})
}
