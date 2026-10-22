package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/testverification"
)

func TestTeamRunnerBookkeepingLaunchesFourFakeMembersOnce(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "scope-queue", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"order", "sequencing_note", "decision"} {
		msg, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: kind, RequestID: "scope-source-" + kind, WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}})
		if err != nil {
			t.Fatal(err)
		}
		req := api.WorkOrderBookkeepingRequest{RequestID: "scope-save-" + kind, AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: f.item.Revision, OrderMessageSeq: f.order, SourceMessageSeq: msg.Seq, Kind: kind, QueueEntryID: q.ID}
		first, err := f.c.SaveWorkOrderBookkeeping(ctx, f.task.ID, f.item.ID, req)
		if err != nil {
			t.Fatal(err)
		}
		second, err := f.c.SaveWorkOrderBookkeeping(ctx, f.task.ID, f.item.ID, req)
		if err != nil || second.ID != first.ID {
			t.Fatalf("bookkeeping retry %+v %v", second, err)
		}
	}
	if current, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || current.Revision != q.Revision || current.ItemRevision != q.ItemRevision || current.State != "queued" || current.OrderMessageSeq != q.OrderMessageSeq {
		t.Fatalf("bookkeeping changed queued entry %+v %v", current, err)
	}
	f.restartHub(t)
	if replay, err := f.c.GetWorkOrderBookkeepingReceipt(ctx, f.task.ID, f.item.ID, "scope-save-decision"); err != nil || replay.QueueEntryID != q.ID || replay.ItemRevision != f.item.Revision {
		t.Fatalf("restart lost bookkeeping receipt %+v %v", replay, err)
	}
	messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var order api.Message
	for _, msg := range messages {
		if msg.Seq == f.order {
			order = msg
		}
	}
	if order.Seq == 0 {
		t.Fatal("order missing")
	}
	spawns := 0
	runner := teamRunner{
		plan: func(_ context.Context, in map[string]any, out *teamLaunchResolved) error {
			out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, f.item, order)
			for _, role := range []string{"lead", "planner", "builder", "reviewer"} {
				out.Plan = append(out.Plan, teamLaunchEntry{Fields: teamLaunchFields{Name: role + "-scope", Role: role, Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}})
			}
			return nil
		},
		spawn: func(_ env, args []string) error {
			spawns++
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i += 2 {
				flags[args[i]] = args[i+1]
			}
			data, err := os.ReadFile(flags["--work-context-file"])
			if err != nil {
				return err
			}
			rev, err := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			if err != nil {
				return err
			}
			seq, err := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			if err != nil {
				return err
			}
			_, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: rev, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: seq}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
	}
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	if spawns != 4 {
		t.Fatalf("fake spawns=%d, want four", spawns)
	}
	current, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || current.State != "running" || current.ItemRevision != f.item.Revision || current.OrderMessageSeq != f.order {
		t.Fatalf("launched queue %+v %v", current, err)
	}
	item, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil || item.Revision != f.item.Revision || item.ScopeRevision != f.item.ScopeRevision {
		t.Fatalf("launch changed scope %+v %v", item, err)
	}
}

func TestTeamRunnerReportsUnconfirmedQueuedScopeAndRetriesAfterIntake(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "pre-upgrade-queue", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// This isolated database models an entry queued before confirmations were
	// introduced. The handler can confirm its unchanged revision in place.
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM work_order_scope_confirmations WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	spawns := 0
	runner := teamRunner{
		plan: func(_ context.Context, in map[string]any, out *teamLaunchResolved) error {
			out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, f.item, api.Message{TaskID: f.task.ID, Seq: f.order, Text: "bounded fixture order"})
			for _, role := range []string{"lead", "planner", "builder", "reviewer"} {
				out.Plan = append(out.Plan, teamLaunchEntry{Fields: teamLaunchFields{Name: role + "-scope", Role: role, Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}})
			}
			return nil
		},
		spawn: func(_ env, args []string) error {
			spawns++
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i += 2 {
				flags[args[i]] = args[i+1]
			}
			data, err := os.ReadFile(flags["--work-context-file"])
			if err != nil {
				return err
			}
			rev, err := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			if err != nil {
				return err
			}
			seq, err := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			if err != nil {
				return err
			}
			_, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: rev, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: seq}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
	}
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err == nil || !strings.Contains(err.Error(), "scope is not confirmed for this exact item revision and order") {
		t.Fatalf("missing actionable scope refusal: %v", err)
	}
	before, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || before.State != "queued" || before.Revision != q.Revision || spawns != 0 {
		t.Fatalf("refusal consumed queue or spawned: %+v, spawns=%d, err=%v", before, spawns, err)
	}
	if _, err := f.c.ConfirmWorkOrderScope(ctx, f.task.ID, f.item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "post-upgrade-intake", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: f.item.Revision, ScopeRevision: f.item.ScopeRevision, OrderMessageSeq: f.order, Complete: true}); err != nil {
		t.Fatal(err)
	}
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	after, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || after.State != "running" || after.ItemRevision != q.ItemRevision || after.OrderMessageSeq != q.OrderMessageSeq || spawns != 4 {
		t.Fatalf("reconfirmed queue did not launch once: %+v, spawns=%d, err=%v", after, spawns, err)
	}
}

func TestTeamRunnerClaimRaceKeepsSilentRetry(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "race-queue", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "other-runner-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "fixture"}); err != nil {
		t.Fatal(err)
	}
	spawns := 0
	runner := teamRunner{spawn: func(env, []string) error { spawns++; return nil }}
	if err := runner.advance(ctx, f.e, f.c, q, "fixture"); err != nil || spawns != 0 {
		t.Fatalf("claim race should retry silently without spawning: err=%v spawns=%d", err, spawns)
	}
}

func TestTeamRunnerScopeRefusalDoesNotStopOtherProjects(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	otherTask, err := f.c.CreateTask(ctx, api.CreateTaskRequest{Name: "other queue on same host"})
	if err != nil {
		t.Fatal(err)
	}
	otherHandler, err := f.c.AddAgent(ctx, otherTask.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "other-db-handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "other-handler", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PostEvent(ctx, otherTask.ID, api.PostEventRequest{AgentID: otherHandler.ID, RunID: otherHandler.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
	otherItem, err := f.c.CreateWorkItem(ctx, otherTask.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "other fixture feature", RequestID: "other-queue-item"})
	if err != nil {
		t.Fatal(err)
	}
	otherOrder, err := f.c.PostMessage(ctx, otherTask.ID, api.PostMessageRequest{Text: "other bounded order", RequestID: "other-queue-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: otherTask.ID, ItemID: otherItem.ID, ItemRevision: otherItem.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ConfirmWorkOrderScope(ctx, otherTask.ID, otherItem.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "other-queue-scope", AgentID: otherHandler.ID, RunID: otherHandler.RunID, ExpectedRevision: otherItem.Revision, ScopeRevision: otherItem.ScopeRevision, OrderMessageSeq: otherOrder.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	type project struct {
		task    api.Task
		item    api.WorkItem
		order   api.Message
		handler api.Agent
		queue   api.TeamQueueEntry
	}
	projects := []project{
		{task: f.task, item: f.item, order: api.Message{TaskID: f.task.ID, Seq: f.order, Text: "bounded fixture order"}, handler: f.handler},
		{task: otherTask, item: otherItem, order: otherOrder, handler: otherHandler},
	}
	for i := range projects {
		p := &projects[i]
		p.queue, err = f.c.TeamQueueAction(ctx, p.task.ID, api.TeamQueueRequest{RequestID: "two-project-queue-" + p.item.ID, Operation: "add", ItemID: p.item.ID, OrderMessageSeq: p.order.Seq, Host: "fixture", Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Host enumeration is ordered by task ID. Make its first project the
	// unconfirmed one so this test detects an early return from tick.
	if projects[1].task.ID < projects[0].task.ID {
		projects[0], projects[1] = projects[1], projects[0]
	}
	first, second := projects[0], projects[1]
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM work_order_scope_confirmations WHERE task_id=? AND item_id=?`, first.task.ID, first.item.ID); err != nil {
		t.Fatal(err)
	}
	byTask := map[string]project{first.task.ID: first, second.task.ID: second}
	spawns := map[string]int{}
	runner := teamRunner{
		plan: func(_ context.Context, in map[string]any, out *teamLaunchResolved) error {
			p := byTask[in["task"].(string)]
			out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, p.item, p.order)
			for _, role := range []string{"lead", "planner", "builder", "reviewer"} {
				out.Plan = append(out.Plan, teamLaunchEntry{Fields: teamLaunchFields{Name: role + "-" + p.item.ID[3:11], Role: role, Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}})
			}
			return nil
		},
		spawn: func(_ env, args []string) error {
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i += 2 {
				flags[args[i]] = args[i+1]
			}
			taskID := flags["--task"]
			p := byTask[taskID]
			spawns[taskID]++
			data, err := os.ReadFile(flags["--work-context-file"])
			if err != nil {
				return err
			}
			rev, err := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			if err != nil {
				return err
			}
			seq, err := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			if err != nil {
				return err
			}
			_, err = f.c.AddAgent(ctx, taskID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: taskID, ItemID: p.item.ID, ItemRevision: rev, WorkOrderMessage: api.MessageReference{TaskID: taskID, Seq: seq}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
	}
	err = runner.tick(ctx, f.e, f.c, "fixture")
	if err == nil || !strings.Contains(err.Error(), first.queue.ID) || !strings.Contains(err.Error(), "scope is not confirmed for this exact item revision and order") {
		t.Fatalf("first project scope error was lost: %v", err)
	}
	firstEntry, err := f.c.GetTeamQueueEntry(ctx, first.task.ID, first.queue.ID)
	if err != nil || firstEntry.State != "queued" || firstEntry.Revision != first.queue.Revision || spawns[first.task.ID] != 0 {
		t.Fatalf("first project changed on refusal: %+v, spawns=%d, err=%v", firstEntry, spawns[first.task.ID], err)
	}
	secondEntry, err := f.c.GetTeamQueueEntry(ctx, second.task.ID, second.queue.ID)
	if err != nil || secondEntry.State != "running" || secondEntry.ItemRevision != second.item.Revision || spawns[second.task.ID] != 4 {
		t.Fatalf("second project did not launch in same tick: %+v, spawns=%d, err=%v", secondEntry, spawns[second.task.ID], err)
	}
	if _, err := f.c.ConfirmWorkOrderScope(ctx, first.task.ID, first.item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "two-project-reconfirm", AgentID: first.handler.ID, RunID: first.handler.RunID, ExpectedRevision: first.item.Revision, ScopeRevision: first.item.ScopeRevision, OrderMessageSeq: first.order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	// Both projects use the same synthetic caller. Refill the test server's
	// write bucket before the second four-member launch.
	time.Sleep(2 * time.Second)
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	firstEntry, err = f.c.GetTeamQueueEntry(ctx, first.task.ID, first.queue.ID)
	if err != nil || firstEntry.State != "running" || firstEntry.ItemRevision != first.item.Revision || spawns[first.task.ID] != 4 || spawns[second.task.ID] != 4 {
		t.Fatalf("reconfirmed project did not launch exactly once: %+v, spawns=%v, err=%v", firstEntry, spawns, err)
	}
}

func TestTeamRunnerTwoItemsWithFakeSpawnsAndCleanup(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	second, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "second", RequestID: "second-item"})
	if err != nil {
		t.Fatal(err)
	}
	secondOrder, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "second bounded order", RequestID: "second-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: second.ID, ItemRevision: second.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	items := []api.WorkItem{f.item, second}
	f.confirmOrder(t, second, secondOrder.Seq)
	orders := []int64{f.order, secondOrder.Seq}
	var queue []api.TeamQueueEntry
	for i, item := range items {
		q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("add-%d", i), Operation: "add", ItemID: item.ID, OrderMessageSeq: orders[i], Host: "fixture", Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		queue = append(queue, q)
	}
	fakeSpawns := 0
	r := teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			itemID := in["item"].(string)
			var item api.WorkItem
			var order int64
			for i, v := range items {
				if v.ID == itemID {
					item = v
					order = orders[i]
				}
			}
			messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
			if err != nil {
				return err
			}
			var msg api.Message
			for _, v := range messages {
				if v.Seq == order {
					msg = v
				}
			}
			if msg.Seq == 0 {
				return fmt.Errorf("order missing")
			}
			out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, item, msg)
			out.Plan = []teamLaunchEntry{{Fields: teamLaunchFields{Name: "lead-" + itemID[3:11], Role: "lead", Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}}}
			return nil
		},
		spawn: func(e env, args []string) error {
			fakeSpawns++
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i += 2 {
				flags[args[i]] = args[i+1]
			}
			data, err := os.ReadFile(flags["--work-context-file"])
			if err != nil {
				return err
			}
			rev, err := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			if err != nil {
				return err
			}
			order, err := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			if err != nil {
				return err
			}
			_, err = f.c.AddAgent(ctx, flags["--task"], api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: flags["--task"], ItemID: flags["--work-item"], ItemRevision: rev, WorkOrderMessage: api.MessageReference{TaskID: flags["--task"], Seq: order}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
		cleanup: func(ctx context.Context, e env, task, id string) error {
			a, err := f.c.GetAgent(ctx, task, id)
			if err != nil {
				return err
			}
			_, err = f.c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: a.RunID})
			return err
		},
	}
	for i := 0; i < 2; i++ {
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		q, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, queue[i].ID)
		if err != nil || q.State != "running" {
			t.Fatalf("launch %d %+v %v", i, q, err)
		}
		if fakeSpawns != i+1 {
			t.Fatalf("spawns %d", fakeSpawns)
		}
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		if fakeSpawns != i+1 {
			t.Fatal("running team spawned twice")
		}
		terminal := "dismissed"
		current, err := f.st.GetWorkItem(ctx, f.task.ID, items[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, current.ID, api.UpdateWorkItemRequest{Revision: current.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			detail, err := f.c.GetTask(ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			request, err := closeTeamSnapshot(detail.Task, detail.Agents, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.CloseItemTeam(ctx, f.task.ID, request); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, queue[i].ID)
		if err != nil || q.State != "finished" {
			t.Fatalf("finish %d %+v %v", i, q, err)
		}
		detail, err := f.c.GetTask(ctx, f.task.ID)
		if err != nil || detail.Task.Orchestrator != "" || detail.Task.Status != api.TaskOpen {
			t.Fatalf("project lost %+v %v", detail.Task, err)
		}
		if i == 0 {
			found := false
			for _, a := range detail.Agents {
				if a.ID == f.handler.ID && a.Status != api.AgentClosed {
					found = true
				}
			}
			if !found {
				t.Fatal("handler closed")
			}
		}
	}
	list, err := f.c.ListTeamQueue(ctx, f.task.ID)
	if err != nil || len(list.Entries) != 2 || strings.Contains(list.Entries[1].State, "failed") {
		t.Fatalf("queue %+v %v", list, err)
	}
	_, _ = json.Marshal(list)
}

func TestTeamRunnerCrashAroundSpawnNeverRespawns(t *testing.T) {
	for _, registered := range []bool{false, true} {
		name := "before-registration"
		if registered {
			name = "after-registration"
		}
		t.Run(name, func(t *testing.T) {
			f := newTeamFixture(t, true)
			ctx := context.Background()
			q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			var order api.Message
			for _, m := range messages {
				if m.Seq == f.order {
					order = m
				}
			}
			bundle := teamCloseCLIContext(t, f.item, order)
			spawns := 0
			r := teamRunner{
				plan: func(_ context.Context, in map[string]any, out *teamLaunchResolved) error {
					out.ItemRouting.WorkContextBundle = bundle
					out.Plan = []teamLaunchEntry{{Fields: teamLaunchFields{Name: "crash-lead", Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}}}
					return nil
				},
				spawn: func(_ env, args []string) error {
					spawns++
					flags := map[string]string{}
					for i := 0; i+1 < len(args); i += 2 {
						flags[args[i]] = args[i+1]
					}
					if registered {
						data, err := os.ReadFile(flags["--work-context-file"])
						if err != nil {
							t.Fatal(err)
						}
						_, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order}, ContextBundle: data}})
						if err != nil {
							t.Fatal(err)
						}
					}
					panic("synthetic process crash")
				},
				owned: func(context.Context, env, api.Agent) error { return nil }, cleanup: func(context.Context, env, string, string) error { return nil },
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("spawn boundary did not crash")
					}
				}()
				_ = r.tick(ctx, f.e, f.c, "fixture")
			}()
			q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			if err != nil || q.State != "launching" {
				t.Fatalf("effect state %+v %v", q, err)
			}
			err = r.tick(ctx, f.e, f.c, "fixture")
			q, readErr := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "failed"
			if registered {
				want = "running"
			}
			if q.State != want || spawns != 1 {
				t.Fatalf("restart state=%s want=%s spawns=%d error=%v", q.State, want, spawns, err)
			}
			if !registered && q.EscalationSeq == 0 {
				t.Fatal("failure did not store owner escalation")
			}
			_ = r.tick(ctx, f.e, f.c, "fixture")
			if spawns != 1 {
				t.Fatal("restart spawned member twice")
			}
		})
	}
}

func TestTeamRunnerFailedCloseEscalatesOnce(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	runID := api.NewID("run")
	journal := teamLaunchJournal{Version: 1, Task: f.task.ID, Item: f.item.ID, Revision: f.item.Revision, Order: f.order, Context: json.RawMessage(`{"version":1}`), Members: []teamLaunchMember{{Fields: teamLaunchFields{AgentID: api.NewID("agt"), Name: "lead", Cwd: t.TempDir()}, State: "unstarted", RunID: runID}}}
	data, _ := json.Marshal(journal)
	q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "freeze", Operation: "freeze", EntryID: q.ID, ExpectedRevision: q.Revision, LaunchJSON: data})
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision})
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "started", Operation: "started", EntryID: q.ID, ExpectedRevision: q.Revision, MemberRunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "running", Operation: "running", EntryID: q.ID, ExpectedRevision: q.Revision})
	if err != nil {
		t.Fatal(err)
	}
	terminal := "dismissed"
	if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	r := teamRunner{}
	if err := r.tick(ctx, f.e, f.c, "fixture"); err == nil {
		t.Fatal("missing close receipt accepted")
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || q.State != "failed" || q.EscalationSeq < 1 {
		t.Fatalf("failed close %+v %v", q, err)
	}
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	again, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || again.EscalationSeq != q.EscalationSeq {
		t.Fatalf("escalation replay %+v %v", again, err)
	}
}

func TestTeamRunnerStaleQueuedItemFailsBeforeSpawn(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	status := "dismissed"
	if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &status}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := (teamRunner{}).tick(ctx, f.e, f.c, "fixture"); err == nil {
		t.Fatal("stale item did not fail")
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || q.State != "failed" || q.EscalationSeq == 0 {
		t.Fatalf("stale entry %+v %v", q, err)
	}
}

// queueCorrectionRunner exercises the actual hub API with fake runtime effects.
func queueCorrectionRunner(t *testing.T, f teamFixture, members int) teamRunner {
	t.Helper()
	ctx := context.Background()
	return teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
			if err != nil {
				return err
			}
			var order api.Message
			for _, m := range messages {
				if m.Seq == f.order {
					order = m
				}
			}
			out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, f.item, order)
			for i := 0; i < members; i++ {
				out.Plan = append(out.Plan, teamLaunchEntry{Fields: teamLaunchFields{Name: fmt.Sprintf("correction-lead-%d", i), Role: "lead", Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}})
			}
			return nil
		},
		spawn: func(e env, args []string) error {
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i += 2 {
				flags[args[i]] = args[i+1]
			}
			data, err := os.ReadFile(flags["--work-context-file"])
			if err != nil {
				return err
			}
			rev, _ := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			order, _ := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			_, err = f.c.AddAgent(ctx, flags["--task"], api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: flags["--task"], ItemID: flags["--work-item"], ItemRevision: rev, WorkOrderMessage: api.MessageReference{TaskID: flags["--task"], Seq: order}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
		cleanup: func(ctx context.Context, e env, task, id string) error {
			a, err := f.c.GetAgent(ctx, task, id)
			if err != nil {
				return err
			}
			_, err = f.c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: a.RunID})
			return err
		},
	}
}

func TestTeamRunnerWaitsForCloseObligationAndRefreshesSnapshot(t *testing.T) {
	for _, changedSnapshot := range []bool{false, true} {
		name := "open-obligation"
		if changedSnapshot {
			name = "changed-snapshot"
		}
		t.Run(name, func(t *testing.T) {
			f := newTeamFixture(t, true)
			ctx := context.Background()
			q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			r := queueCorrectionRunner(t, f, 1)
			if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
				t.Fatal(err)
			}
			detail, err := f.c.GetTask(ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			var lead api.Agent
			for _, a := range detail.Agents {
				if a.Name == "correction-lead-0" {
					lead = a
				}
			}
			if lead.ID == "" {
				t.Fatal("lead missing")
			}
			terminal := "dismissed"
			if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
				t.Fatal(err)
			}
			var obligationID string
			if changedSnapshot {
				req, err := closeTeamSnapshot(detail.Task, detail.Agents, "", "")
				if err != nil {
					t.Fatal(err)
				}
				req.RequestID = "queue-close-stale-snapshot"
				data, _ := json.Marshal(req)
				q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
				if err != nil {
					t.Fatal(err)
				}
				q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "freeze-stale", Operation: "close", EntryID: q.ID, ExpectedRevision: q.Revision, CloseJSON: data})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: lead.ID, RunID: lead.RunID, Kind: api.EventExited}); err != nil {
					t.Fatal(err)
				}
			} else {
				msg, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "confirm delivery", To: lead.ID, RequestID: "owner-request"})
				if err != nil {
					t.Fatal(err)
				}
				obligations, err := f.c.ListObligationsFrom(ctx, f.task.ID, lead.ID, msg.Seq, msg.Seq)
				if err != nil || len(obligations) != 1 {
					t.Fatalf("obligation %+v %v", obligations, err)
				}
				obligationID = obligations[0].ID
			}
			if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
				t.Fatal(err)
			}
			q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			if err != nil || q.State != "running" || (len(q.CloseJSON) == 0) != changedSnapshot || q.EscalationSeq != 0 {
				t.Fatalf("waiting queue %+v %v", q, err)
			}
			if !changedSnapshot {
				waitingRevision := q.Revision
				waitingClose := string(q.CloseJSON)
				db, err := sql.Open("sqlite", f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var receiptsBefore int
				if err := db.QueryRow(`SELECT count(*) FROM team_queue_requests WHERE task_id=?`, f.task.ID).Scan(&receiptsBefore); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 12; i++ {
					if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
						t.Fatal(err)
					}
					q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
					if err != nil || q.Revision != waitingRevision || string(q.CloseJSON) != waitingClose || q.EscalationSeq != 0 {
						t.Fatalf("waiting tick %d changed frozen receipt %+v %v", i, q, err)
					}
				}
				var receiptsAfter int
				if err := db.QueryRow(`SELECT count(*) FROM team_queue_requests WHERE task_id=?`, f.task.ID).Scan(&receiptsAfter); err != nil {
					t.Fatal(err)
				}
				if receiptsAfter != receiptsBefore {
					t.Fatalf("waiting gate wrote %d extra queue receipts", receiptsAfter-receiptsBefore)
				}
				if _, err := f.c.CancelObligation(ctx, f.task.ID, obligationID, api.ObligationCancelRequest{Reason: "fixture resolved", RequestID: "cancel"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
				t.Fatal(err)
			}
			q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			if err != nil || q.State != "finished" {
				t.Fatalf("finished queue %+v %v", q, err)
			}
		})
	}
}

func TestTeamRunnerSameItemLeadReplacementKeepsReservation(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := queueCorrectionRunner(t, f, 2)
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || q.State != "running" {
		t.Fatalf("launch %+v %v", q, err)
	}
	replacement := "correction-lead-1"
	detailBefore, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range detailBefore.Agents {
		if agent.Name == replacement {
			if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: agent.ID, RunID: agent.RunID, Kind: api.EventRunning}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.st.UpdateTask(ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &replacement}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatalf("same-item replacement %v", err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "manual-race", Operation: "manual", ItemID: f.item.ID, OrderMessageSeq: f.order}); err == nil {
		t.Fatal("manual launch crossed running reservation")
	}
	other := "unbound-outside-lead"
	if _, err := f.st.UpdateTask(ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &other}, api.Caller{Node: "fixture", User: "owner"}); err == nil {
		t.Fatal("unbound lead crossed reservation")
	}
	terminal := "dismissed"
	if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	closeReq, err := closeTeamSnapshot(detail.Task, detail.Agents, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.CloseItemTeam(ctx, f.task.ID, closeReq); err != nil {
		t.Fatal(err)
	}
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || q.State != "finished" {
		t.Fatalf("finished after replacement %+v %v", q, err)
	}
}

func TestTeamRunnerUnregisteredCrashReleaseRequiresSynchronizedHostProof(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	host := spawn.Host()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: host, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := queueCorrectionRunner(t, f, 1)
	spawns := 0
	r.spawn = func(env, []string) error { spawns++; panic("test crash before registration") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("crash did not occur")
			}
		}()
		_ = r.tick(ctx, f.e, f.c, host)
	}()
	if err := r.tick(ctx, f.e, f.c, host); err == nil {
		t.Fatal("uncertain effect did not fail closed")
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || q.State != "failed" || spawns != 1 {
		t.Fatalf("failed exact attempt %+v spawns=%d err=%v", q, spawns, err)
	}
	var frozen teamLaunchJournal
	if err := json.Unmarshal(q.LaunchJSON, &frozen); err != nil || len(frozen.Members) != 1 {
		t.Fatalf("frozen uncertain attempt %+v %v", frozen, err)
	}
	empty := ""
	if _, err := f.c.UpdateTask(ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &empty}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "no-proof", Operation: "release", EntryID: q.ID, ExpectedRevision: q.Revision}); err == nil {
		t.Fatal("missing proof released uncertain spawn")
	}
	lock, err := queueLaunchLock(f.e.hub, f.task.ID, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err == nil {
		t.Fatal("active host launch lock allowed release")
	}
	unlockQueueLaunch(lock)
	name := frozen.Members[0].Fields.Name
	if _, err := startupTmux(ctx, "new-session", "-d", "-s", name); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err == nil {
		t.Fatal("name-conflicting local session allowed release")
	}
	if _, err := startupTmux(ctx, "kill-session", "-t", name); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err != nil {
		t.Fatalf("verified absent session release: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err != nil {
		t.Fatalf("release receipt retry: %v", err)
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || q.ReleasedAt == "" || q.State != "failed" || spawns != 1 {
		t.Fatalf("released history %+v spawns=%d err=%v", q, spawns, err)
	}
}

// Handler arm waits (wi_fc1396aef8a72a06): a drawn arm with no free handler
// and every arm at a provider limit are ordinary waits, not failures.
func TestTeamRunnerClaimRaceConflictArmWaits(t *testing.T) {
	for _, message := range []string{
		"conflict: arm S: no free handler in the drawn arm",
		"conflict: every handler arm is at a provider limit, so there is no free handler in the drawn arm",
	} {
		if !claimRaceConflict(message) {
			t.Fatalf("not a wait: %s", message)
		}
	}
	if claimRaceConflict("conflict: handler arm policy refused (template_mismatch): x") {
		t.Fatal("a policy refusal is not a wait")
	}
	f := newTeamFixture(t, true)
	ctx := context.Background()
	if _, err := f.c.SetHandlerArmPolicy(ctx, f.task.ID, api.HandlerArmPolicyRequest{RequestID: "arms", Enabled: true, Seed: "K", TemplateDigest: strings.Repeat("a", 64),
		Arms: []api.HandlerArm{{ID: "S", Runtime: "claude", Model: "claude-sonnet-5-5", Reasoning: "high", Weight: 1}, {ID: "O", Runtime: "codex", Model: "gpt-6.1-sol", Reasoning: "high", Weight: 1}}}); err != nil {
		t.Fatal(err)
	}
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "arm-queue", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	spawns := 0
	runner := teamRunner{spawn: func(env, []string) error { spawns++; return nil }}
	if err := runner.advance(ctx, f.e, f.c, q, "fixture"); err != nil || spawns != 0 {
		t.Fatalf("arm wait should retry silently: err=%v spawns=%d", err, spawns)
	}
	if after, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || after.State != "queued" || after.Failure != "" {
		t.Fatalf("arm wait changed the entry %+v %v", after, err)
	}
}

// q2: a runner pass reads only each project's active entries, however much
// history the project has.
func TestTeamRunnerTickReadsActiveEntries(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	insertFinishedQueueRows(t, f, 1, 3)
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "active-queue", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	e, c, queries := recordTeamQueueListings(t, f)
	if err := queueCorrectionRunner(t, f, 1).tick(ctx, e, c, "fixture"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || got.State != "running" {
		t.Fatalf("entry after tick %+v %v", got.State, err)
	}
	got := queries()
	if len(got) < 3 {
		t.Fatalf("tick, claim and launch should each read the queue: %v", got)
	}
	for _, query := range got {
		if query != "view=active" {
			t.Fatalf("runner listing query %q, want view=active (all %v)", query, got)
		}
	}
}

// launchFaultHub serves a fixture's store through a hub that refuses chosen
// team queue operations and counts every team queue action by operation. It
// also counts task reads under "get_task", can refuse the project listing
// with listFault, and can rewrite both team queue listings with listing.
type launchFaultHub struct {
	e         env
	c         *api.Client
	mu        sync.Mutex
	faults    map[string]*api.HTTPError
	counts    map[string]int
	listFault *api.HTTPError
	listing   func(*api.TeamQueueList)
}

func newLaunchFaultHub(t *testing.T, f teamFixture) *launchFaultHub {
	t.Helper()
	h := &launchFaultHub{faults: map[string]*api.HTTPError{}, counts: map[string]int{}}
	inner := server.New(f.st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "team-fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/tasks/") && strings.Count(r.URL.Path, "/") == 3 {
			h.mu.Lock()
			h.counts["get_task"]++
			h.mu.Unlock()
		}
		if r.Method == "GET" && (strings.HasSuffix(r.URL.Path, "/team-queue") || r.URL.Path == "/v1/team-queues") {
			h.mu.Lock()
			fault, rewrite := h.listFault, h.listing
			h.mu.Unlock()
			if fault != nil && strings.HasSuffix(r.URL.Path, "/team-queue") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(fault.Status)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: fault.Msg})
				return
			}
			if rewrite != nil {
				rec := httptest.NewRecorder()
				inner.ServeHTTP(rec, r)
				var list api.TeamQueueList
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
					w.WriteHeader(rec.Code)
					_, _ = w.Write(rec.Body.Bytes())
					return
				}
				rewrite(&list)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(list)
				return
			}
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/team-queue/actions") {
			body, _ := io.ReadAll(r.Body)
			var req api.TeamQueueRequest
			_ = json.Unmarshal(body, &req)
			h.mu.Lock()
			h.counts[req.Operation]++
			fault := h.faults[req.Operation]
			h.mu.Unlock()
			if fault != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(fault.Status)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: fault.Msg})
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h.e, h.c = env{hub: srv.URL, task: f.task.ID}, c
	return h
}

func (h *launchFaultHub) refuse(operation string, status int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if status == 0 {
		delete(h.faults, operation)
		return
	}
	h.faults[operation] = &api.HTTPError{Status: status, Msg: msg}
}

func (h *launchFaultHub) setListing(fault *api.HTTPError, rewrite func(*api.TeamQueueList)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listFault, h.listing = fault, rewrite
}

func (h *launchFaultHub) count(operation string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[operation]
}

// launchNotices counts the Board notices with this subject for the entry.
func launchNotices(t *testing.T, f teamFixture, subject, entry string) []api.Message {
	t.Helper()
	messages, err := f.c.ListMessages(context.Background(), f.task.ID, 0, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range messages {
		if m.Envelope != nil && m.Envelope.Subject == subject && m.Envelope.Refs["entry"] == entry {
			out = append(out, m)
		}
	}
	return out
}

// launchRetryFixture queues the fixture item behind a fault hub, with a
// one-member fake launch, an isolated retry book and a fake retry clock.
func launchRetryFixture(t *testing.T) (teamFixture, *launchFaultHub, api.TeamQueueEntry, *teamRunner, *time.Time, *int) {
	t.Helper()
	f := newTeamFixture(t, true)
	hub := newLaunchFaultHub(t, f)
	q, err := hub.c.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: "retry-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	spawns := 0
	runner := queueCorrectionRunner(t, f, 1)
	spawn := runner.spawn
	runner.spawn = func(e env, args []string) error {
		spawns++
		return spawn(e, args)
	}
	runner.retries = &launchRetryBook{}
	runner.now = func() time.Time { return now }
	return f, hub, q, &runner, &now, &spawns
}

const launchFailedSubject = "Team queue failed and requires owner action"

// wi_a3ca8b64d12365c2 a1, a2: a permanent refusal during launch fails the
// entry after exactly three backed-off attempts with one failure notice; a
// stale revision never fails it.
func TestTeamRunnerPermanentLaunchRefusal(t *testing.T) {
	cases := []struct {
		name, op string
		status   int
		msg      string
	}{
		{"freeze-413", "freeze", 413, "body too large"},
		{"freeze-400", "freeze", 400, "invalid request"},
		{"freeze-422", "freeze", 422, "unprocessable entity"},
		{"attempt-413", "attempt", 413, "body too large"},
		{"freeze-409-retry-differs", "freeze", 409, "conflict: team queue retry differs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, hub, q, runner, now, spawns := launchRetryFixture(t)
			ctx := context.Background()
			hub.refuse(tc.op, tc.status, tc.msg)
			tick := func() { _ = runner.tick(ctx, hub.e, hub.c, "fixture") }
			tick()
			if hub.count(tc.op) != 1 {
				t.Fatalf("first tick sent %d %s requests", hub.count(tc.op), tc.op)
			}
			tick() // inside the 15s backoff
			*now = now.Add(14 * time.Second)
			tick()
			if hub.count(tc.op) != 1 {
				t.Fatalf("ticks inside the backoff sent %d %s requests", hub.count(tc.op), tc.op)
			}
			for want := 2; want <= 3; want++ {
				*now = now.Add(time.Minute)
				tick()
				if hub.count(tc.op) != want {
					t.Fatalf("after %d refusals: %d %s requests", want-1, hub.count(tc.op), tc.op)
				}
			}
			got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			wantFailure := fmt.Sprintf("launch %s refused permanently by the hub after 3 attempts: hub: %d %s", tc.op, tc.status, tc.msg)
			if err != nil || got.State != "failed" || got.Failure != wantFailure || got.EscalationSeq == 0 {
				t.Fatalf("entry after three refusals: state=%s failure=%q escalation=%d %v", got.State, got.Failure, got.EscalationSeq, err)
			}
			notices := launchNotices(t, f, launchFailedSubject, q.ID)
			if len(notices) != 1 || notices[0].Seq != got.EscalationSeq || notices[0].Envelope.Refs["item"] != f.item.ID || !strings.Contains(notices[0].Envelope.Body.Text, fmt.Sprintf("%d %s", tc.status, tc.msg)) {
				t.Fatalf("failure notices %+v", notices)
			}
			if *spawns != 0 {
				t.Fatalf("spawns=%d", *spawns)
			}
			fails := hub.count("fail")
			for i := 0; i < 5; i++ {
				*now = now.Add(time.Minute)
				tick()
			}
			if hub.count(tc.op) != 3 || hub.count("fail") != fails || len(launchNotices(t, f, launchFailedSubject, q.ID)) != 1 {
				t.Fatalf("after failure: %d %s, %d fail, notices %d", hub.count(tc.op), tc.op, hub.count("fail")-fails, len(launchNotices(t, f, launchFailedSubject, q.ID)))
			}
		})
	}
	t.Run("freeze-409-entry-revision-changed", func(t *testing.T) {
		f, hub, q, runner, now, spawns := launchRetryFixture(t)
		ctx := context.Background()
		hub.refuse("freeze", 409, "conflict: entry revision changed")
		for i := 0; i < 5; i++ {
			if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err == nil {
				t.Fatalf("refusal %d was silent", i+1)
			}
			*now = now.Add(5 * time.Minute)
		}
		got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil || got.State != "launching" || got.Failure != "" || hub.count("freeze") != 5 || hub.count("fail") != 0 {
			t.Fatalf("stale revision failed the entry: state=%s failure=%q %v freezes=%d", got.State, got.Failure, err, hub.count("freeze"))
		}
		hub.refuse("freeze", 0, "")
		if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		got, err = hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil || got.State != "running" || *spawns != 1 || len(launchNotices(t, f, launchFailedSubject, q.ID)) != 0 {
			t.Fatalf("recovered launch: state=%s %v spawns=%d", got.State, err, *spawns)
		}
	})
}

// wi_a3ca8b64d12365c2 a3, a5: a transient launch error backs off 15s, 30s,
// 1m, 2m, 4m then 5m, never fails the entry, and posts one stall notice with
// the last error that a relay restart does not repeat; the list shows the
// stall, and the launch finishes once the hub recovers.
func TestTeamRunnerTransientLaunchError(t *testing.T) {
	f, hub, q, runner, now, spawns := launchRetryFixture(t)
	ctx := context.Background()
	f.st.SetQueueStallTiming(0, 50*time.Millisecond)
	runner.stallGrace = 50 * time.Millisecond
	hub.refuse("freeze", 503, "service unavailable")
	tick := func() { _ = runner.tick(ctx, hub.e, hub.c, "fixture") }
	tick()
	if hub.count("freeze") != 1 {
		t.Fatalf("first tick sent %d freezes", hub.count("freeze"))
	}
	time.Sleep(100 * time.Millisecond) // past the hub's stall grace
	for i, gap := range []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		*now = now.Add(gap - time.Second)
		tick()
		if hub.count("freeze") != i+1 {
			t.Fatalf("attempt %d came before its %s gap", i+2, gap)
		}
		*now = now.Add(time.Second)
		tick()
		if hub.count("freeze") != i+2 {
			t.Fatalf("attempt %d missing after its %s gap: %d freezes", i+2, gap, hub.count("freeze"))
		}
	}
	got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || got.State != "launching" || got.Failure != "" || got.EscalationSeq != 0 || hub.count("fail") != 0 || len(launchNotices(t, f, launchFailedSubject, q.ID)) != 0 {
		t.Fatalf("transient error failed the entry: state=%s failure=%q escalation=%d %v", got.State, got.Failure, got.EscalationSeq, err)
	}
	list, err := hub.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Stall == nil || list.Entries[0].Stall.BlockerEntryID != q.ID || !strings.HasPrefix(list.Entries[0].BlockReason, "Stalled: "+q.ID) {
		t.Fatalf("list lacks the launch stall: %d entries %v", len(list.Entries), err)
	}
	const stallSubject = "A team queue launch has made no progress"
	notices := launchNotices(t, f, stallSubject, q.ID)
	if len(notices) != 1 {
		t.Fatalf("stall notices %d", len(notices))
	}
	env := notices[0].Envelope
	if env.Refs["item"] != f.item.ID || env.Refs["cause"] != api.StallNothingRunning || !strings.Contains(env.Body.Text, "Last launch error on the runner: freeze: hub: 503 service unavailable") {
		t.Fatalf("stall notice %+v", env)
	}
	// A relay restart forgets its book and its posted notices.
	runner.retries = &launchRetryBook{}
	postedStallNotices.Range(func(k, _ any) bool { postedStallNotices.Delete(k); return true })
	asked := hub.count("stall_notice")
	for i := 0; i < 3; i++ {
		*now = now.Add(5 * time.Minute)
		tick()
	}
	if n := len(launchNotices(t, f, stallSubject, q.ID)); n != 1 || hub.count("stall_notice") != asked+1 {
		t.Fatalf("restart posted %d stall notices from %d requests", n, hub.count("stall_notice")-asked)
	}
	hub.refuse("freeze", 0, "")
	*now = now.Add(5 * time.Minute)
	if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	got, err = hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || got.State != "running" || *spawns != 1 {
		t.Fatalf("recovered launch: state=%s %v spawns=%d", got.State, err, *spawns)
	}
}

// wi_a3ca8b64d12365c2 a4: only a refusal the same write cannot change is
// permanent, and the backoff doubles from 15 seconds to a 5-minute cap.
func TestLaunchRetryClassificationAndDelay(t *testing.T) {
	step := func(err error) error { return &launchStepError{op: "freeze", revision: 2, err: err} }
	for _, tc := range []struct {
		err       error
		permanent bool
	}{
		{&api.HTTPError{Status: 400, Msg: "invalid request"}, true},
		{&api.HTTPError{Status: 404, Msg: "not found"}, true},
		{&api.HTTPError{Status: 413, Msg: "body too large"}, true},
		{&api.HTTPError{Status: 422, Msg: "unprocessable"}, true},
		{&api.HTTPError{Status: 409, Msg: "conflict: team queue retry differs"}, true},
		{step(&api.HTTPError{Status: 413, Msg: "body too large"}), true},
		{&api.HTTPError{Status: 409, Msg: "conflict: entry revision changed"}, false},
		{&api.HTTPError{Status: 409, Msg: "conflict: project is not launchable"}, false},
		{&api.HTTPError{Status: 401, Msg: "unauthorized"}, false},
		{&api.HTTPError{Status: 403, Msg: "forbidden"}, false},
		{&api.HTTPError{Status: 408, Msg: "timeout"}, false},
		{&api.HTTPError{Status: 429, Msg: "too many requests"}, false},
		{&api.HTTPError{Status: 500, Msg: "internal"}, false},
		{&api.HTTPError{Status: 502, Msg: "bad gateway"}, false},
		{&api.HTTPError{Status: 503, Msg: "unavailable"}, false},
		{&api.HTTPError{Status: 504, Msg: "gateway timeout"}, false},
		{step(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), false},
		{step(context.DeadlineExceeded), false},
	} {
		if got := launchErrorPermanent(tc.err); got != tc.permanent {
			t.Errorf("launchErrorPermanent(%v)=%v, want %v", tc.err, got, tc.permanent)
		}
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, d := range want {
		if got := launchRetryDelay(i + 1); got != d {
			t.Errorf("launchRetryDelay(%d)=%s, want %s", i+1, got, d)
		}
	}
	if got := launchRetryDelay(64); got != 5*time.Minute {
		t.Errorf("launchRetryDelay(64)=%s", got)
	}
	// The bound counts one refusal in a row: another outcome restarts it,
	// and an unwrapped error never counts.
	book := &launchRetryBook{}
	now := time.Now()
	refusal := step(&api.HTTPError{Status: 413, Msg: "body too large"})
	for i, err := range []error{refusal, refusal, step(&api.HTTPError{Status: 503}), refusal, refusal, &api.HTTPError{Status: 413}, refusal, refusal} {
		if got := book.record("k", err, now); got != nil {
			t.Fatalf("record %d reached the bound early", i)
		}
	}
	if got := book.record("k", refusal, now); got == nil || got.op != "freeze" || got.revision != 2 {
		t.Fatalf("third refusal in a row %+v", got)
	}
}

// wi_a3ca8b64d12365c2 a3, review b1: a relaunch of an item whose earlier team
// closed hours ago posts no stall notice inside the grace after its claim,
// and one once the grace has passed.
func TestTeamRunnerTransientLaunchErrorRelaunch(t *testing.T) {
	f, hub, q, runner, now, _ := launchRetryFixture(t)
	ctx := context.Background()
	const grace = 3 * time.Second
	f.st.SetQueueStallTiming(0, grace)
	runner.stallGrace = grace
	messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var order api.Message
	for _, m := range messages {
		if m.Seq == f.order {
			order = m
		}
	}
	old, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "old-lead", Host: "fixture", Session: "old-lead", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order}, ContextBundle: teamCloseCLIContext(t, f.item, order)}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE agents SET status='closed', last_event_at=? WHERE id=?`, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339Nano), old.ID); err != nil {
		t.Fatal(err)
	}
	hub.refuse("freeze", 503, "service unavailable")
	const stallSubject = "A team queue launch has made no progress"
	for i := 0; i < 3; i++ {
		_ = runner.tick(ctx, hub.e, hub.c, "fixture")
		*now = now.Add(5 * time.Minute)
	}
	got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || got.State != "launching" || hub.count("freeze") != 3 {
		t.Fatalf("relaunch: state=%s freezes=%d %v", got.State, hub.count("freeze"), err)
	}
	claimed, err := time.Parse(time.RFC3339Nano, got.UpdatedAt)
	if err != nil || time.Since(claimed) >= grace {
		t.Skipf("host too slow to observe inside the %s grace (%v)", grace, err)
	}
	list, err := hub.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Stall != nil || hub.count("stall_notice") != 0 || len(launchNotices(t, f, stallSubject, q.ID)) != 0 {
		t.Fatalf("relaunch stalled inside the grace: stall=%+v notices=%d %v", list.Entries[0].Stall, hub.count("stall_notice"), err)
	}
	time.Sleep(time.Until(claimed.Add(grace + 100*time.Millisecond)))
	for i := 0; i < 3; i++ {
		*now = now.Add(5 * time.Minute)
		_ = runner.tick(ctx, hub.e, hub.c, "fixture")
	}
	if notices := launchNotices(t, f, stallSubject, q.ID); len(notices) != 1 || !strings.Contains(notices[0].Envelope.Body.Text, "freeze: hub: 503 service unavailable") {
		t.Fatalf("relaunch stall notices past the grace: %+v", notices)
	}
}

// smallLaneSpawner admits each spawned member through the hub like the real
// spawn, recording its name and roster size.
func smallLaneSpawner(f teamFixture, names *[]string) func(env, []string) error {
	return func(_ env, args []string) error {
		flags := map[string]string{}
		for i := 0; i+1 < len(args); i += 2 {
			flags[args[i]] = args[i+1]
		}
		if flags["--planned-team-members"] != "3" {
			return fmt.Errorf("--planned-team-members=%q, want 3", flags["--planned-team-members"])
		}
		*names = append(*names, flags["--name"])
		data, err := os.ReadFile(flags["--work-context-file"])
		if err != nil {
			return err
		}
		rev, _ := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
		order, _ := strconv.ParseInt(flags["--work-order-message"], 10, 64)
		_, err = f.c.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: flags["--runtime"], Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: rev, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: order}, ContextBundle: data}})
		return err
	}
}

// wi_f8d48780626165cc a5: a queued small bug launches the real embedded
// three-seat plan, lead first, and the lead becomes the orchestrator.
func TestTeamRunnerLaunchesSmallBugAsThreeSeats(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "small-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Template: "small", Host: "fixture", Cwd: t.TempDir(), Ownership: []string{"hub/cmd/tt/fix.go", "hub/cmd/tt/fix_test.go"}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	runner := productionTeamRunner()
	runner.spawn = smallLaneSpawner(f, &names)
	runner.owned = func(context.Context, env, api.Agent) error { return nil }
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	suffix := f.item.ID[len(f.item.ID)-8:]
	if want := []string{"lead-" + suffix, "builder-" + suffix, "reviewer-" + suffix}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("spawned %v, want %v", names, want)
	}
	current, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || current.State != "running" || current.Template != "small" {
		t.Fatalf("small entry %+v %v", current, err)
	}
	var journal teamLaunchJournal
	if err := json.Unmarshal(current.LaunchJSON, &journal); err != nil || len(journal.Members) != 3 || journal.Members[0].Fields.Role != "Small-change lead and verifier" {
		t.Fatalf("frozen small team %+v %v", journal.Members, err)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil || detail.Task.Orchestrator != "lead-"+suffix {
		t.Fatalf("orchestrator %q %v", detail.Task.Orchestrator, err)
	}
}

// wi_f8d48780626165cc a5: the runner refuses a small entry whose item is not a
// bug before it plans or spawns anything.
func TestTeamRunnerSmallFeatureFailsWithoutSpawn(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "planned-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir(), Ownership: []string{"hub/cmd/tt/fix.go"}})
	if err != nil {
		t.Fatal(err)
	}
	// The store refuses this admission; a direct row edit stands in for an
	// entry saved before the kind rule or by another writer.
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE team_queue_entries SET template='small' WHERE id=?`, q.ID); err != nil {
		t.Fatal(err)
	}
	var names []string
	planned := false
	runner := teamRunner{
		plan: func(context.Context, map[string]any, *teamLaunchResolved) error {
			planned = true
			return errors.New("planned a refused entry")
		},
		spawn: smallLaneSpawner(f, &names),
		owned: func(context.Context, env, api.Agent) error { return nil },
	}
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err == nil || !strings.Contains(err.Error(), "the small-change lane launches only bugs; this item is a feature: requeue it as Planned delivery") {
		t.Fatalf("small feature tick: %v", err)
	}
	if planned || len(names) != 0 {
		t.Fatalf("refused entry planned=%v spawned %v", planned, names)
	}
	current, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || current.State != "failed" || !strings.Contains(current.Failure, "launches only bugs") || current.EscalationSeq == 0 {
		t.Fatalf("small feature entry %+v %v", current, err)
	}
}

// capFillers registers n plain open agents on the fixture project.
func capFillers(t *testing.T, f teamFixture, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		if _, err := f.c.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "fixture", Session: name, Runtime: "codex"}); err != nil {
			t.Fatal(err)
		}
	}
}

// closeCapFillers closes every agent whose name starts with the prefix.
func closeCapFillers(t *testing.T, f teamFixture, prefix string) {
	t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE agents SET status='closed' WHERE task_id=? AND name LIKE ?`, f.task.ID, prefix+"-%"); err != nil {
		t.Fatal(err)
	}
}

// wi_91e0cf6fa1dedbef a7: the runner holds a queued entry the project agent
// cap refuses, without failing it, and launches it once seats free.
func TestTeamRunnerHoldsQueuedEntryAtProjectAgentCap(t *testing.T) {
	f := newTeamFixture(t, true)
	hub := newLaunchFaultHub(t, f)
	ctx := context.Background()
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cap-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The handler and one worker are open; a feature team needs six seats.
	f.st.MaxAgents = 7
	capFillers(t, f, "cap-worker", 1)
	spawns := 0
	runner := queueCorrectionRunner(t, f, 2)
	spawn := runner.spawn
	runner.spawn = func(e env, args []string) error {
		spawns++
		return spawn(e, args)
	}
	runner.retries = &launchRetryBook{}
	for i := 0; i < 2; i++ {
		if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
			t.Fatalf("held tick %d: %v", i, err)
		}
	}
	held, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || held.State != "queued" || held.Revision != q.Revision || held.Failure != "" || spawns != 0 || hub.count("claim") != 2 || hub.count("fail") != 0 || len(launchNotices(t, f, launchFailedSubject, q.ID)) != 0 {
		t.Fatalf("held entry state=%s revision=%d failure=%q spawns=%d claims=%d fails=%d %v", held.State, held.Revision, held.Failure, spawns, hub.count("claim"), hub.count("fail"), err)
	}
	list, err := hub.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].BlockReason != "project agent cap: 2 open + 6 seats > 7" {
		t.Fatalf("held reason %+v %v", list.Entries, err)
	}
	closeCapFillers(t, f, "cap-worker")
	if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || got.State != "running" || spawns != 2 || hub.count("fail") != 0 {
		t.Fatalf("launch after seats freed: state=%s spawns=%d %v", got.State, spawns, err)
	}
}

// wi_91e0cf6fa1dedbef a8: a registration the hub refuses at the project agent
// cap holds the launch. The entry is not failed and no owner notice is posted;
// the error names the cap with the open count; the launch finishes once a seat
// frees, without respawning the started member.
func TestTeamRunnerRegistrationCapHoldsLaunch(t *testing.T) {
	f := newTeamFixture(t, true)
	hub := newLaunchFaultHub(t, f)
	ctx := context.Background()
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cap-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	runner := queueCorrectionRunner(t, f, 2)
	runner.retries = &launchRetryBook{}
	runner.now = func() time.Time { return now }
	register := runner.spawn
	attempts := map[string]int{}
	registered := map[string]int{}
	filled := false
	runner.spawn = func(e env, args []string) error {
		name := ""
		for i := 0; i+1 < len(args); i += 2 {
			if args[i] == "--name" {
				name = args[i+1]
			}
		}
		attempts[name]++
		if name == "correction-lead-1" && !filled {
			// Other registrations fill the project after this member's
			// attempt was recorded: the handler, the first member and 30
			// more make the 32 the hub allows.
			filled = true
			capFillers(t, f, "late-worker", 30)
		}
		// cmdSpawn registers before it creates a session and wraps the
		// hub's refusal the same way.
		if err := register(e, args); err != nil {
			return fmt.Errorf("register agent: %w", err)
		}
		registered[name]++
		return nil
	}
	const capError = "spawn correction-lead-1: project agent cap: 32 open + 1 seats > 32: register agent: hub: 409 limit reached"
	if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err == nil || !strings.Contains(err.Error(), capError) {
		t.Fatalf("refused registration: %v", err)
	}
	key := launchRetryKey(hub.c.Base, f.task.ID, q.ID)
	held := func(step string) api.TeamQueueEntry {
		t.Helper()
		got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil || got.State != "launching" || got.Failure != "" || got.EscalationSeq != 0 || hub.count("fail") != 0 || len(launchNotices(t, f, launchFailedSubject, q.ID)) != 0 {
			t.Fatalf("%s: state=%s failure=%q escalation=%d fails=%d %v", step, got.State, got.Failure, got.EscalationSeq, hub.count("fail"), err)
		}
		var journal teamLaunchJournal
		if err := json.Unmarshal(got.LaunchJSON, &journal); err != nil || len(journal.Members) != 2 || journal.Members[0].State != "started" || journal.Members[1].State != "unstarted" {
			t.Fatalf("%s: members %+v %v", step, journal.Members, err)
		}
		return got
	}
	held("after the refusal")
	if got := runner.retries.lastError(key); got != capError {
		t.Fatalf("last launch error %q", got)
	}
	if hub.count("unattempt") != 1 || attempts["correction-lead-1"] != 1 {
		t.Fatalf("unattempts=%d spawn attempts=%v", hub.count("unattempt"), attempts)
	}
	// Inside the backoff nothing is tried.
	now = now.Add(launchRetryFirstDelay - time.Second)
	if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err != nil || hub.count("attempt") != 2 {
		t.Fatalf("tick inside the backoff: %v attempts=%d", err, hub.count("attempt"))
	}
	// Past it the project is still full: the hub refuses the attempt, so
	// nothing is spawned and the member stays unstarted.
	now = now.Add(time.Second)
	if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err == nil || !strings.Contains(err.Error(), "attempt: hub: 409 conflict: project agent cap: 32 open + 1 seats > 32") {
		t.Fatalf("attempt at a full project: %v", err)
	}
	held("after the refused attempt")
	if attempts["correction-lead-1"] != 1 {
		t.Fatalf("spawned at a full project: %v", attempts)
	}
	closeCapFillers(t, f, "late-worker")
	now = now.Add(launchRetryMaxDelay)
	if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || got.State != "running" || got.Failure != "" || hub.count("fail") != 0 || len(launchNotices(t, f, launchFailedSubject, q.ID)) != 0 {
		t.Fatalf("launch after a seat freed: state=%s failure=%q %v", got.State, got.Failure, err)
	}
	if attempts["correction-lead-0"] != 1 || registered["correction-lead-0"] != 1 || attempts["correction-lead-1"] != 2 || registered["correction-lead-1"] != 1 {
		t.Fatalf("spawn attempts %v registered %v", attempts, registered)
	}
}

// Automatic handler provisioning (wi_01b6d3afed81167c, order #27052). The
// fixture's hub, relay directory and tmux socket are temporary, and the
// persistent launch seam is substituted, so no handler is started.

const provisionTestPrompt = "fixture handler prompt"

type provisionRunnerFixture struct {
	teamFixture
	host    string
	waiting api.TeamQueueEntry
	spawns  []api.AddAgentRequest
	runner  teamRunner
	// lost makes the substituted launch return without registering, as when
	// the handler's session dies before it reaches the hub.
	lost bool
}

// newProvisionRunnerFixture is a parallel project with limit 2 whose one
// recorded handler is leased by a running team on another host, a second
// entry that waits only for a handler, and the given saved launch spec.
func newProvisionRunnerFixture(t *testing.T, specArgs ...string) *provisionRunnerFixture {
	t.Helper()
	p := &provisionRunnerFixture{teamFixture: newTeamFixture(t, true), host: spawn.Host()}
	f := p.teamFixture
	ctx := context.Background()
	handler, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler-recorded", Role: api.AgentRoleDatabaseHandler, Host: p.host, Session: "fixture-handler",
		Runtime: "claude", TemplateDigest: handlerTemplateDigest(provisionTestPrompt), HandlerModel: "claude-sonnet-5-5", HandlerReasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "2")
	second, secondOrder := queueFixtureItem(t, f, "provision-second")
	add := func(key string, item api.WorkItem, order int64, owns string) api.TeamQueueEntry {
		q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "add", ItemID: item.ID, OrderMessageSeq: order, Host: p.host, Cwd: t.TempDir(), Repository: repo, BaseCommit: head, Ownership: []string{owns}})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	busy := add("provision-busy", f.item, f.order, "client")
	p.waiting = add("provision-waiting", second, secondOrder, "hub")
	// A running team on another host holds the only handler; this host's
	// runner leaves that entry alone. The fixture's own handler recorded no
	// launch template, so it is closed: the recorded one is the project's.
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE agents SET status='closed' WHERE id=?`, f.handler.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE team_queue_entries SET state='running',host='elsewhere',handler_id=?,handler_run_id=?,handler_lease_generation=1 WHERE id=?`, handler.ID, handler.RunID, busy.ID); err != nil {
		t.Fatal(err)
	}
	if len(specArgs) > 0 {
		spec, err := newHandlerSpec(f.c.Base, f.task.ID, specArgs)
		if err != nil {
			t.Fatal(err)
		}
		if err := saveHandlerSpec(spec); err != nil {
			t.Fatal(err)
		}
	}
	prior := ensurePersistent
	ensurePersistent = func(ctx context.Context, c *api.Client, task string, req api.AddAgentRequest, _ spawn.Options) (api.Agent, error) {
		// Stand in for the launch: register the handler and report it online.
		p.spawns = append(p.spawns, req)
		if p.lost {
			return api.Agent{ID: req.AgentID, TaskID: task, Name: req.Name}, nil
		}
		req.Session = "fixture-" + req.Name
		a, err := c.AddAgent(ctx, task, req)
		if err != nil {
			return a, err
		}
		_, err = c.PostEvent(ctx, task, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventRunning})
		return a, err
	}
	t.Cleanup(func() { ensurePersistent = prior })
	p.runner = teamRunner{
		plan: func(context.Context, map[string]any, *teamLaunchResolved) error {
			return errors.New("no team launch in this test")
		},
		spawn: cmdSpawn,
		owned: func(context.Context, env, api.Agent) error { return nil },
	}
	return p
}

func (p *provisionRunnerFixture) listed(t *testing.T) api.TeamQueueEntry {
	t.Helper()
	q, err := p.c.GetTeamQueueEntry(context.Background(), p.task.ID, p.waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := p.c.ListTeamQueuePage(context.Background(), p.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Entries {
		if e.ID == q.ID {
			return e
		}
	}
	t.Fatalf("entry %s not listed", q.ID)
	return q
}

// a11: the runner reserves one handler for the waiting entry and starts it
// once from the saved spec; the claim then leases it.
func TestTeamRunnerProvisionsHandlerFromSavedSpec(t *testing.T) {
	p := newProvisionRunnerFixture(t, "--run", "claude", "--model", "claude-sonnet-5-5", "--reasoning", "high", "--prompt", provisionTestPrompt)
	ctx := context.Background()
	before := p.listed(t)
	if before.HandlerNeed == nil || !before.HandlerNeed.Provision || before.BlockReason != "No free database handler: 1 of 1 leased, limit 2; the runner is adding one" {
		t.Fatalf("waiting entry %+v %q", before.HandlerNeed, before.BlockReason)
	}
	if err := p.runner.tick(ctx, p.e, p.c, p.host); err != nil {
		t.Fatal(err)
	}
	if len(p.spawns) != 1 {
		t.Fatalf("spawn calls %d, want 1", len(p.spawns))
	}
	wantID := handlerProvisionAgentID(p.c.Base, p.task.ID, fmt.Sprintf("queue-provision-%s-%d-1", p.waiting.ID, p.waiting.Revision))
	got := p.spawns[0]
	if got.AgentID != wantID || got.Role != api.AgentRoleDatabaseHandler || got.Runtime != "claude" || got.HandlerModel != "claude-sonnet-5-5" || got.HandlerReasoning != "high" ||
		got.TemplateDigest != handlerTemplateDigest(provisionTestPrompt) || got.Name != "db-handler-auto-"+wantID[len(wantID)-6:] || got.ParentAgentID != "" {
		t.Fatalf("spawned handler %+v, want agent %s", got, wantID)
	}
	notices := 0
	messages, err := p.c.ListMessages(ctx, p.task.ID, 0, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Envelope != nil && m.Envelope.Subject == "Automatic handler provision" {
			notices++
			if m.Envelope.Refs["agent"] != wantID || m.Envelope.Refs["entry"] != p.waiting.ID || m.Intervention != nil {
				t.Fatalf("notice %+v", m)
			}
		}
	}
	if notices != 1 {
		t.Fatalf("provision notices %d", notices)
	}
	// The registered handler is free, so the entry no longer waits and a
	// further pass adds nothing.
	p.runner.provisionHandler(ctx, p.e, p.c, api.TeamQueueList{Entries: []api.TeamQueueEntry{before}}, p.host)
	if len(p.spawns) != 1 {
		t.Fatalf("spawn calls after a replayed pass %d", len(p.spawns))
	}
	if after := p.listed(t); after.HandlerNeed != nil || after.BlockReason != "" {
		t.Fatalf("after registration %+v %q", after.HandlerNeed, after.BlockReason)
	}
	claimed, err := p.c.TeamQueueAction(ctx, p.task.ID, api.TeamQueueRequest{RequestID: "provision-claim", Operation: "claim", EntryID: p.waiting.ID, ExpectedRevision: p.waiting.Revision, Host: p.host})
	if err != nil || claimed.State != "launching" || claimed.HandlerID != wantID {
		t.Fatalf("claim after provision %+v %v", claimed, err)
	}
}

// a11: nothing is started when the hub says no handler may be added: the
// switch is off, the saved spec differs, or no spec is saved.
func TestTeamRunnerDoesNotProvisionWhenRefused(t *testing.T) {
	t.Run("switch off", func(t *testing.T) {
		p := newProvisionRunnerFixture(t, "--run", "claude", "--model", "claude-sonnet-5-5", "--reasoning", "high", "--prompt", provisionTestPrompt)
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(p.e, []string{"provision", "--auto", "off"}) }); err != nil {
			t.Fatal(err)
		}
		if err := p.runner.tick(context.Background(), p.e, p.c, p.host); err != nil {
			t.Fatal(err)
		}
		got := p.listed(t)
		if len(p.spawns) != 0 || got.HandlerNeed == nil || got.HandlerNeed.Provision || !strings.HasSuffix(got.BlockReason, "Fix: tt team queue provision --task "+p.task.ID+" --auto on") {
			t.Fatalf("spawns %d need %+v %q", len(p.spawns), got.HandlerNeed, got.BlockReason)
		}
	})
	t.Run("spec differs", func(t *testing.T) {
		p := newProvisionRunnerFixture(t, "--run", "claude", "--model", "claude-opus-5-5", "--reasoning", "high", "--prompt", provisionTestPrompt)
		for pass := 0; pass < 2; pass++ {
			if err := p.runner.tick(context.Background(), p.e, p.c, p.host); err != nil {
				t.Fatal(err)
			}
		}
		got := p.listed(t)
		if len(p.spawns) != 0 || got.HandlerNeed == nil || got.HandlerNeed.Provision || !got.HandlerNeed.Refused ||
			!strings.Contains(got.BlockReason, "the saved launch spec on "+p.host+" is claude/claude-opus-5-5/high") ||
			!strings.Contains(got.BlockReason, `Fix: PROMPT="$(cat PROMPT_FILE)" && tt handler spec --task `+p.task.ID+` -- --run 'claude' --runtime claude --model claude-sonnet-5-5 --reasoning high --prompt "$PROMPT" (`) {
			t.Fatalf("spawns %d need %+v %q", len(p.spawns), got.HandlerNeed, got.BlockReason)
		}
		// The owner saves the matching spec; the next pass adds the handler.
		spec, err := newHandlerSpec(p.c.Base, p.task.ID, []string{"--run", "claude", "--model", "claude-sonnet-5-5", "--reasoning", "high", "--prompt", provisionTestPrompt})
		if err != nil {
			t.Fatal(err)
		}
		if err := saveHandlerSpec(spec); err != nil {
			t.Fatal(err)
		}
		if err := p.runner.tick(context.Background(), p.e, p.c, p.host); err != nil || len(p.spawns) != 1 {
			t.Fatalf("after the corrected spec: %v, spawns %d", err, len(p.spawns))
		}
	})
	// f4: a run command given by path yields a runtime no arm can have. The
	// hub refuses it as a spec that differs instead of rejecting the request.
	t.Run("spec runtime is a path", func(t *testing.T) {
		p := newProvisionRunnerFixture(t, "--run", "/opt/bin/claude", "--model", "claude-sonnet-5-5", "--reasoning", "high", "--prompt", provisionTestPrompt)
		if err := p.runner.tick(context.Background(), p.e, p.c, p.host); err != nil {
			t.Fatal(err)
		}
		got := p.listed(t)
		if len(p.spawns) != 0 || got.HandlerNeed == nil || !got.HandlerNeed.Refused || !strings.Contains(got.BlockReason, "the saved launch spec on "+p.host+" is /opt/bin/claude/claude-sonnet-5-5/high") ||
			strings.Contains(got.BlockReason, "--run '") || !strings.Contains(got.BlockReason, "add --run with the claude launch command") {
			t.Fatalf("spawns %d need %+v %q", len(p.spawns), got.HandlerNeed, got.BlockReason)
		}
	})
	t.Run("no spec", func(t *testing.T) {
		p := newProvisionRunnerFixture(t)
		if err := p.runner.tick(context.Background(), p.e, p.c, p.host); err != nil {
			t.Fatal(err)
		}
		got := p.listed(t)
		if len(p.spawns) != 0 || got.HandlerNeed == nil || !got.HandlerNeed.Refused || !strings.Contains(got.BlockReason, "the saved launch spec on "+p.host+" is missing") {
			t.Fatalf("spawns %d need %+v %q", len(p.spawns), got.HandlerNeed, got.BlockReason)
		}
	})
}

// a8 (review b1): a started handler that never registers is abandoned after
// ten minutes and the runner, with its own retry identity, reserves and
// starts a new one.
func TestTeamRunnerRetriesAbandonedHandlerProvision(t *testing.T) {
	p := newProvisionRunnerFixture(t, "--run", "claude", "--model", "claude-sonnet-5-5", "--reasoning", "high", "--prompt", provisionTestPrompt)
	p.lost = true
	ctx := context.Background()
	db, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := func(state string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM handler_provisions WHERE task_id=? AND state=?`, p.task.ID, state).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	notices := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=? AND envelope<>'' AND json_extract(envelope,'$.subject')='Automatic handler provision'`, p.task.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// While the reservation is pending, further passes start nothing.
	for pass := 0; pass < 3; pass++ {
		if err := p.runner.tick(ctx, p.e, p.c, p.host); err != nil {
			t.Fatal(err)
		}
	}
	first := handlerProvisionAgentID(p.c.Base, p.task.ID, fmt.Sprintf("queue-provision-%s-%d-1", p.waiting.ID, p.waiting.Revision))
	if len(p.spawns) != 1 || p.spawns[0].AgentID != first || rows("reserved") != 1 || notices() != 1 {
		t.Fatalf("pending: spawns %d reserved %d notices %d", len(p.spawns), rows("reserved"), notices())
	}
	if got := p.listed(t); got.HandlerNeed == nil || got.HandlerNeed.Provision || got.HandlerNeed.Attempt != 1 || got.HandlerNeed.AgentID != first {
		t.Fatalf("pending need %+v", got.HandlerNeed)
	}
	// Ten minutes pass without the handler registering.
	if _, err := db.Exec(`UPDATE handler_provisions SET created_at=? WHERE task_id=? AND state='reserved'`, time.Now().UTC().Add(-11*time.Minute).Format(time.RFC3339Nano), p.task.ID); err != nil {
		t.Fatal(err)
	}
	if got := p.listed(t); got.HandlerNeed == nil || !got.HandlerNeed.Provision || got.HandlerNeed.Attempt != 2 {
		t.Fatalf("expired need %+v", got.HandlerNeed)
	}
	if err := p.runner.tick(ctx, p.e, p.c, p.host); err != nil {
		t.Fatal(err)
	}
	second := handlerProvisionAgentID(p.c.Base, p.task.ID, fmt.Sprintf("queue-provision-%s-%d-2", p.waiting.ID, p.waiting.Revision))
	if len(p.spawns) != 2 || p.spawns[1].AgentID != second || second == first {
		t.Fatalf("retry spawns %d, second agent %s (first %s)", len(p.spawns), second, first)
	}
	if rows("abandoned") != 1 || rows("reserved") != 1 || notices() != 2 {
		t.Fatalf("after retry: abandoned %d reserved %d notices %d", rows("abandoned"), rows("reserved"), notices())
	}
	var abandoned, reserved string
	if err := db.QueryRow(`SELECT agent_id FROM handler_provisions WHERE task_id=? AND state='abandoned'`, p.task.ID).Scan(&abandoned); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT agent_id FROM handler_provisions WHERE task_id=? AND state='reserved'`, p.task.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if abandoned != first || reserved != second {
		t.Fatalf("abandoned %s reserved %s", abandoned, reserved)
	}
	// The second handler registers; the entry stops waiting and leases it.
	p.lost = false
	agent, err := p.c.AddAgent(ctx, p.task.ID, func() api.AddAgentRequest { r := p.spawns[1]; r.Session = "fixture-second"; return r }())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.c.PostEvent(ctx, p.task.ID, api.PostEventRequest{AgentID: agent.ID, RunID: agent.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
	claimed, err := p.c.TeamQueueAction(ctx, p.task.ID, api.TeamQueueRequest{RequestID: "retry-claim", Operation: "claim", EntryID: p.waiting.ID, ExpectedRevision: p.waiting.Revision, Host: p.host})
	if err != nil || claimed.HandlerID != second || rows("registered") != 1 || len(p.spawns) != 2 {
		t.Fatalf("claim after retry %+v %v registered %d", claimed, err, rows("registered"))
	}
}

// T21 (wi_b863e669073858f4, a12): a dead primary rotation moves a launching
// entry's lease and rewrites the handler identity in its stored launch plan.
// The runner's own frozen-launch identity check must accept that entry and
// carry on with the successor, and must still refuse a plan that names
// another handler. The dead handler is a fixture row: nothing is probed,
// stopped or started.
func TestTeamRunnerFrozenLaunchFollowsDeadPrimaryHandoff(t *testing.T) {
	for _, tampered := range []bool{false, true} {
		name := "moved plan accepted"
		if tampered {
			name = "plan naming another handler refused"
		}
		t.Run(name, func(t *testing.T) {
			f, hub, q, runner, now, spawns := launchRetryFixture(t)
			ctx := context.Background()
			by := api.Caller{Node: "team-fixture", User: "owner"}
			var handlerFlags []string
			spawn := runner.spawn
			runner.spawn = func(e env, args []string) error {
				for i := 0; i+1 < len(args); i += 2 {
					if args[i] == "--team-handler-id" {
						handlerFlags = append(handlerFlags, args[i+1])
					}
				}
				return spawn(e, args)
			}
			// The launch is frozen and then stops before any member starts.
			hub.refuse("attempt", 503, "service unavailable")
			_ = runner.tick(ctx, hub.e, hub.c, "fixture")
			frozen, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			if err != nil || frozen.State != "launching" || len(frozen.LaunchJSON) == 0 || frozen.HandlerID != f.handler.ID || *spawns != 0 {
				t.Fatalf("frozen launch: %+v %v spawns=%d", frozen, err, *spawns)
			}
			var before teamLaunchJournal
			if err = json.Unmarshal(frozen.LaunchJSON, &before); err != nil || before.HandlerID != f.handler.ID || before.HandlerRunID != frozen.HandlerRunID {
				t.Fatalf("frozen plan: %+v %v", before, err)
			}
			// The handler dies holding that lease: the hub last heard from it 11
			// minutes ago and its host confirms it gone, twice.
			db, err := sql.Open("sqlite", f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, time.Now().UTC().Add(-11*time.Minute).Format(time.RFC3339Nano), f.handler.ID); err != nil {
				t.Fatal(err)
			}
			evidence := func() *api.HandlerDeathEvidence {
				return &api.HandlerDeathEvidence{Host: "fixture", AgentID: f.handler.ID, RunID: frozen.HandlerRunID, ObservedAt: time.Now().UTC(), SessionName: "fixture-handler",
					SessionState: api.HandlerDeathStateGone, PID: 4242, ProcessStarted: "Wed Oct  7 06:00:00 2026", ProcessState: api.HandlerDeathStateGone}
			}
			detail, err := hub.c.GetTask(ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.st.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: "dead-prepare", ExpectedHandlerRevision: detail.Task.HandlerRevision,
				OldAgentID: f.handler.ID, OldRunID: frozen.HandlerRunID, SuccessorAgentID: api.NewID("agt"), SuccessorName: api.HandlerSuccessorName(f.handler.Name, detail.Task.HandlerRevision),
				Reason: api.HandlerRotationReasonDeadPrimary, Trigger: api.HandlerRotationTriggerRunner, DeathEvidence: evidence()}, by)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			successor, err := hub.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Name: r.SuccessorName, Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "fixture-handler-r2", Runtime: "codex"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = hub.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: successor.ID, RunID: successor.RunID, Kind: api.EventRunning}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Millisecond) // the second observation is later than the first
			if r, err = f.st.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationCommit, RequestID: "rotation-commit-" + r.ID, RotationID: r.ID, DeathEvidence: evidence()}, by); err != nil {
				t.Fatalf("commit: %v", err)
			}
			moved, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			var after teamLaunchJournal
			if err != nil || json.Unmarshal(moved.LaunchJSON, &after) != nil || moved.State != "launching" || moved.HandlerID != successor.ID || moved.HandlerRunID != successor.RunID ||
				after.HandlerID != successor.ID || after.HandlerRunID != successor.RunID || after.HandlerLeaseGeneration != moved.HandlerLeaseGeneration ||
				r.Receipt.LeasesMoved != 1 || !r.Handoff.LiveLeases[0].LaunchPlanRewritten {
				t.Fatalf("moved entry %+v plan %+v receipt %+v %v", moved, after, r.Receipt, err)
			}
			// Only the handler identity changed in the plan.
			after.HandlerID, after.HandlerRunID, after.HandlerLeaseGeneration = before.HandlerID, before.HandlerRunID, before.HandlerLeaseGeneration
			if restored, _ := json.Marshal(after); string(restored) != string(frozen.LaunchJSON) {
				t.Fatalf("the plan changed beyond the handler identity:\n%s\n%s", restored, frozen.LaunchJSON)
			}
			if tampered {
				// A stored plan that names a handler other than the entry's lease.
				other := strings.Replace(string(moved.LaunchJSON), successor.ID, f.handler.ID, 1)
				if _, err = db.Exec(`UPDATE team_queue_entries SET launch_json=? WHERE id=?`, other, q.ID); err != nil {
					t.Fatal(err)
				}
			}
			hub.refuse("attempt", 0, "")
			*now = now.Add(10 * time.Minute)
			tickErr := runner.tick(ctx, hub.e, hub.c, "fixture")
			got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tampered {
				if got.State != "failed" || !strings.Contains(got.Failure, "frozen team identity conflicts with queue entry") || *spawns != 0 {
					t.Fatalf("a plan naming another handler: state=%s failure=%q spawns=%d", got.State, got.Failure, *spawns)
				}
				return
			}
			if tickErr != nil || got.State != "running" || got.Failure != "" || *spawns != 1 || got.HandlerID != successor.ID {
				t.Fatalf("launch after the handoff: state=%s failure=%q spawns=%d handler=%s %v", got.State, got.Failure, *spawns, got.HandlerID, tickErr)
			}
			if len(handlerFlags) != 1 || handlerFlags[0] != successor.ID {
				t.Fatalf("members were briefed with handler %v, want the successor %s", handlerFlags, successor.ID)
			}
		})
	}
}

// slowListingHub serves a fixture's store through a hub whose team queue
// listings can stop answering. A slow listing is held until its caller gives
// up, and the client gives a listing a short time limit while the hub is
// slow, so the runner meets a real timed-out request without a real wait.
type slowListingHub struct {
	e          env
	c          *api.Client
	task       string
	slowActive atomic.Bool
	slowHost   atomic.Bool
	mu         sync.Mutex
	active     int
	host       int
}

func (h *slowListingHub) slow(r *http.Request) bool {
	if r.Method != "GET" {
		return false
	}
	return (h.slowActive.Load() && r.URL.Path == "/v1/tasks/"+h.task+"/team-queue") || (h.slowHost.Load() && r.URL.Path == "/v1/team-queues")
}

func (h *slowListingHub) RoundTrip(r *http.Request) (*http.Response, error) {
	if !h.slow(r) {
		return http.DefaultTransport.RoundTrip(r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
	defer cancel()
	return http.DefaultTransport.RoundTrip(r.WithContext(ctx))
}

// listings reports how many active and host listings reached the hub.
func (h *slowListingHub) listings() (active, host int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active, h.host
}

func newSlowListingHub(t *testing.T, f teamFixture) *slowListingHub {
	t.Helper()
	resetQueueListings()
	t.Cleanup(resetQueueListings)
	h := &slowListingHub{task: f.task.ID}
	inner := server.New(f.st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "team-fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			h.mu.Lock()
			switch r.URL.Path {
			case "/v1/tasks/" + h.task + "/team-queue":
				h.active++
			case "/v1/team-queues":
				h.host++
			}
			h.mu.Unlock()
		}
		if h.slow(r) {
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP.Transport = h
	h.e, h.c = env{hub: srv.URL, task: f.task.ID}, c
	return h
}

// slowListingNotices lists the project's slow listing notices in order.
func slowListingNotices(t *testing.T, f teamFixture) []api.Message {
	t.Helper()
	messages, err := f.c.ListMessages(context.Background(), f.task.ID, 0, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range messages {
		if m.Envelope != nil && m.Envelope.Subject == queueListingSlowTitle {
			out = append(out, m)
		}
	}
	return out
}

// wi_765e7089fd176b08 f3: a timed-out active listing does not blind the tick.
// It advances a running team from the last good listing, tries the listing
// again only after a growing delay, and tells the owner helper once per
// episode.
func TestTeamRunnerSlowListingReusesLastGoodAndNoticesOnce(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	hub := newSlowListingHub(t, f)
	if _, err := f.st.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "fixture", Session: "owner", Runtime: "claude", RequestID: "slow-listing-helper"}, api.Caller{Node: "team-fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "slow-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := queueCorrectionRunner(t, f, 1)
	r.now = func() time.Time { return now }
	tick := func(what string) {
		t.Helper()
		if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	entry := func() api.TeamQueueEntry {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Two live ticks: the first launches the team, the second lists it running.
	tick("launch tick")
	tick("running tick")
	if got := entry(); got.State != "running" || len(got.CloseJSON) != 0 {
		t.Fatalf("entry before the slow episode: %s, close frozen %v", got.State, len(got.CloseJSON) != 0)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lead, helper api.Agent
	for _, a := range detail.Agents {
		switch {
		case a.Name == "correction-lead-0":
			lead = a
		case a.Role == api.AgentRoleOwnerHelper:
			helper = a
		}
	}
	if lead.ID == "" || helper.ID == "" {
		t.Fatalf("lead %q or owner helper %q missing", lead.ID, helper.ID)
	}
	// The item ends while the lead still owes a reply, so the next step of
	// the running team is to freeze its close and wait.
	terminal := "dismissed"
	if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	msg, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "confirm delivery", To: lead.ID, RequestID: "slow-owner-request"})
	if err != nil {
		t.Fatal(err)
	}
	obligations, err := f.c.ListObligationsFrom(ctx, f.task.ID, lead.ID, msg.Seq, msg.Seq)
	if err != nil || len(obligations) != 1 {
		t.Fatalf("obligation %+v %v", obligations, err)
	}

	// Episode one: the active listing stops answering.
	activeBefore, hostBefore := hub.listings()
	hub.slowActive.Store(true)
	tick("first slow tick")
	if got := entry(); got.State != "running" || len(got.CloseJSON) == 0 {
		t.Fatalf("the slow tick did not advance the running entry from the last good listing: %s, close frozen %v", got.State, len(got.CloseJSON) != 0)
	}
	now = now.Add(time.Second)
	tick("second slow tick, inside the backoff")
	now = now.Add(queueListingRetryFirst)
	tick("third slow tick, after the backoff")
	// Three slow ticks made two live attempts: the second tick fell inside
	// the six-second delay and asked the hub for no listing at all.
	if active, host := hub.listings(); active-activeBefore != 2 || host-hostBefore != 2 {
		t.Fatalf("three slow ticks made %d active and %d host listing calls, want 2 and 2", active-activeBefore, host-hostBefore)
	}
	// The delay doubled: six more seconds is still inside it.
	now = now.Add(queueListingRetryFirst)
	tick("fourth slow tick, inside the doubled backoff")
	if active, _ := hub.listings(); active-activeBefore != 2 {
		t.Fatalf("a tick inside the doubled delay made listing call %d", active-activeBefore)
	}
	notices := slowListingNotices(t, f)
	if len(notices) != 1 || notices[0].To != helper.ID || notices[0].Envelope.Kind != api.EnvelopeKindNotice || notices[0].Envelope.To != helper.Name ||
		!strings.Contains(notices[0].Text, "last good listing") || !strings.Contains(notices[0].Text, f.task.ID) || !strings.Contains(notices[0].Text, "fixture") {
		t.Fatalf("slow ticks posted %d notices, want one to the owner helper %s: %+v", len(notices), helper.ID, notices)
	}

	// Recovery: the next due attempt answers, and three good ticks in a
	// row end the episode. Each lists live, with no delay between them.
	hub.slowActive.Store(false)
	now = now.Add(queueListingRetryMax)
	activeBefore, _ = hub.listings()
	tick("recovery tick")
	tick("second tick after recovery")
	tick("third tick after recovery")
	if active, _ := hub.listings(); active-activeBefore != 3 {
		t.Fatalf("three ticks after recovery made %d active listing calls, want one each", active-activeBefore)
	}
	if got := entry(); got.State != "running" {
		t.Fatalf("entry after recovery: %s", got.State)
	}
	if n := len(slowListingNotices(t, f)); n != 1 {
		t.Fatalf("%d notices after recovery, want still one", n)
	}

	// Episode two: the reply arrives, the listing is slow again, and the
	// running team still closes and finishes from the last good listing.
	if _, err := f.c.CancelObligation(ctx, f.task.ID, obligations[0].ID, api.ObligationCancelRequest{Reason: "fixture resolved", RequestID: "slow-cancel"}); err != nil {
		t.Fatal(err)
	}
	hub.slowActive.Store(true)
	now = now.Add(time.Minute)
	tick("second episode, first tick")
	if got := entry(); got.State != "finished" {
		t.Fatalf("second episode did not finish the running entry from the last good listing: %s", got.State)
	}
	// The entry it knows has moved on; later slow ticks leave it alone.
	now = now.Add(time.Second)
	tick("second episode, second tick")
	notices = slowListingNotices(t, f)
	if len(notices) != 2 || notices[1].To != helper.ID || notices[0].Seq == notices[1].Seq {
		t.Fatalf("second episode: %d notices, want a second one to the owner helper: %+v", len(notices), notices)
	}
}

// wi_765e7089fd176b08 f3: an old listing starts no launch. r5 (f1): past ten
// minutes it is still used, and still starts none.
func TestTeamRunnerSlowListingStartsNoLaunch(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	hub := newSlowListingHub(t, f)
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "slow-queued-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	spawns := 0
	r := queueCorrectionRunner(t, f, 1)
	r.now = func() time.Time { return now }
	spawn := r.spawn
	r.spawn = func(e env, args []string) error {
		spawns++
		return spawn(e, args)
	}
	state := func() string {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}
	// Seed a good listing of the queued entry without launching it: the
	// project is paused for that one tick.
	pause := func(op string) {
		t.Helper()
		db, err := sql.Open("sqlite", f.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`UPDATE tasks SET pause_state=? WHERE id=?`, op, f.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	pause("paused")
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	pause(api.ProjectPauseActive)
	if spawns != 0 || state() != "queued" {
		t.Fatalf("seed tick: %d spawns, entry %s; want a queued entry listed and not launched", spawns, state())
	}

	// Both listings stop answering: the tick works from the last good ones,
	// where the entry is queued, and launches nothing.
	hub.slowActive.Store(true)
	hub.slowHost.Store(true)
	for i, step := range []time.Duration{0, time.Second, queueListingRetryFirst, 2 * queueListingRetryFirst} {
		now = now.Add(step)
		if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
			t.Fatalf("slow tick %d on the last good listing: %v", i, err)
		}
		if spawns != 0 || state() != "queued" {
			t.Fatalf("slow tick %d on the last good listing: %d spawns, entry %s; want no launch", i, spawns, state())
		}
	}
	if n := len(slowListingNotices(t, f)); n != 1 {
		t.Fatalf("%d slow listing notices on the last good listing, want one", n)
	}

	// Thirty minutes on, the last good listing is still used: the tick
	// reports no error and still launches nothing.
	now = now.Add(30 * time.Minute)
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatalf("tick on a listing 30 minutes old: %v", err)
	}
	if spawns != 0 || state() != "queued" {
		t.Fatalf("tick on a listing 30 minutes old: %d spawns, entry %s; want no launch", spawns, state())
	}
	if n := len(slowListingNotices(t, f)); n != 1 {
		t.Fatalf("%d slow listing notices 30 minutes into the episode, want still one", n)
	}

	// A live listing launches the entry.
	hub.slowActive.Store(false)
	hub.slowHost.Store(false)
	now = now.Add(queueListingRetryMax)
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	if spawns != 1 || state() != "running" {
		t.Fatalf("live tick after the episode: %d spawns, entry %s; want the launch", spawns, state())
	}
}

// slowRunningTeam queues the fixture's item through a slow listing hub and
// ticks it to a running team, with an owner helper registered.
func slowRunningTeam(t *testing.T, f teamFixture, hub *slowListingHub, r teamRunner) (api.TeamQueueEntry, api.Agent, api.Agent) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.st.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "fixture", Session: "owner", Runtime: "claude", RequestID: "slow-listing-helper"}, api.Caller{Node: "team-fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "slow-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, what := range []string{"launch tick", "running tick"} {
		if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	if q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || q.State != "running" {
		t.Fatalf("entry before the slow episode: %s %v", q.State, err)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lead, helper api.Agent
	for _, a := range detail.Agents {
		switch {
		case a.Name == "correction-lead-0":
			lead = a
		case a.Role == api.AgentRoleOwnerHelper:
			helper = a
		}
	}
	if lead.ID == "" || helper.ID == "" {
		t.Fatalf("lead %q or owner helper %q missing", lead.ID, helper.ID)
	}
	return q, lead, helper
}

// r5 (wi_100a2639af8b6946): thirty minutes after the last good listing, with
// both listings slow, the tick still advances a running team from that
// listing and reports no error.
func TestTeamRunnerSlowListingStillAdvancesPastTenMinutes(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	hub := newSlowListingHub(t, f)
	now := time.Now()
	r := queueCorrectionRunner(t, f, 1)
	r.now = func() time.Time { return now }
	q, lead, _ := slowRunningTeam(t, f, hub, r)
	terminal := "dismissed"
	if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	msg, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "confirm delivery", To: lead.ID, RequestID: "slow-owner-request"})
	if err != nil {
		t.Fatal(err)
	}
	obligations, err := f.c.ListObligationsFrom(ctx, f.task.ID, lead.ID, msg.Seq, msg.Seq)
	if err != nil || len(obligations) != 1 {
		t.Fatalf("obligation %+v %v", obligations, err)
	}
	entry := func() api.TeamQueueEntry {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	hub.slowActive.Store(true)
	hub.slowHost.Store(true)
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatalf("first slow tick: %v", err)
	}
	if got := entry(); got.State != "running" || len(got.CloseJSON) == 0 {
		t.Fatalf("first slow tick: %s, close frozen %v", got.State, len(got.CloseJSON) != 0)
	}
	// The lead's reply arrives 30 minutes into the episode: the team still
	// closes and finishes from the last good listing.
	if _, err := f.c.CancelObligation(ctx, f.task.ID, obligations[0].ID, api.ObligationCancelRequest{Reason: "fixture resolved", RequestID: "slow-cancel"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatalf("tick 30 minutes into the slow episode: %v", err)
	}
	if got := entry(); got.State != "finished" {
		t.Fatalf("30 minutes into the slow episode the running entry is %s, want finished from the last good listing", got.State)
	}
	if n := len(slowListingNotices(t, f)); n != 1 {
		t.Fatalf("%d slow listing notices, want one", n)
	}
}

// r5 (wi_a560e272cfae1c7d): a relay whose first host listing times out has no
// last good copy and knows no project from the hub. It still tells the owner
// helper bound on this host, once per episode; with no such binding it posts
// nothing and reports only the slow listing.
func TestTeamRunnerSlowHostListingWithNoCopyNoticesBoundHelper(t *testing.T) {
	for _, bound := range []bool{true, false} {
		t.Run(fmt.Sprintf("bound=%v", bound), func(t *testing.T) {
			f := newTeamFixture(t, true)
			ctx := context.Background()
			hub := newSlowListingHub(t, f)
			if _, err := f.st.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "fixture", Session: "owner", Runtime: "claude", RequestID: "slow-listing-helper"}, api.Caller{Node: "team-fixture", User: "owner"}); err != nil {
				t.Fatal(err)
			}
			detail, err := f.c.GetTask(ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			var helper api.Agent
			for _, a := range detail.Agents {
				if a.Role == api.AgentRoleOwnerHelper {
					helper = a
				}
			}
			if helper.ID == "" {
				t.Fatal("owner helper missing")
			}
			if bound {
				// Two bindings of this hub and one of another hub: one
				// notice for the project, none for the other hub's.
				for i, b := range []runtimeBinding{
					{Hub: hub.e.hub, Task: f.task.ID, Agent: helper.ID, Run: helper.RunID, Thread: "11111111-2222-3333-4444-555555555555", Runtime: "claude", Session: "owner", Role: api.AgentRoleOwnerHelper},
					{Hub: hub.e.hub + "/", Task: f.task.ID, Agent: api.NewID("agt"), Run: helper.RunID, Thread: "11111111-2222-3333-4444-555555555556", Runtime: "claude", Session: "owner", Role: api.AgentRoleOwnerHelper},
					{Hub: "http://other.invalid", Task: api.NewID("tsk"), Agent: api.NewID("agt"), Run: helper.RunID, Thread: "11111111-2222-3333-4444-555555555557", Runtime: "claude", Session: "owner", Role: api.AgentRoleOwnerHelper},
					{Hub: hub.e.hub, Task: api.NewID("tsk"), Agent: api.NewID("agt"), Run: helper.RunID, Thread: "11111111-2222-3333-4444-555555555558", Runtime: "claude", Session: "member"},
				} {
					if !validBinding(b) {
						t.Fatalf("binding %d is not valid: %+v", i, b)
					}
					if err := writePrivateJSON(filepath.Join(relayDir(), bindingKey(b)+".binding.json"), b); err != nil {
						t.Fatal(err)
					}
				}
			}
			now := time.Now()
			r := queueCorrectionRunner(t, f, 1)
			r.now = func() time.Time { return now }
			hub.slowHost.Store(true)
			_, hostBefore := hub.listings()
			for i, step := range []time.Duration{0, time.Second, queueListingRetryFirst, 2 * queueListingRetryFirst, 4 * queueListingRetryFirst, queueListingRetryMax} {
				now = now.Add(step)
				err := r.tick(ctx, hub.e, hub.c, "fixture")
				if err == nil || !queueListingTimedOut(err) || !strings.Contains(err.Error(), "no last good listing") || strings.Contains(err.Error(), "\n") {
					t.Fatalf("slow tick %d = %v, want only the slow listing error", i, err)
				}
			}
			// Six ticks, five live attempts: the second fell inside the delay.
			if _, host := hub.listings(); host-hostBefore != 5 {
				t.Fatalf("six slow ticks made %d host listing calls, want 5", host-hostBefore)
			}
			notices := slowListingNotices(t, f)
			if !bound {
				if len(notices) != 0 {
					t.Fatalf("with no owner helper bound here %d notices were posted: %+v", len(notices), notices)
				}
				return
			}
			if len(notices) != 1 || notices[0].To != helper.ID || notices[0].Envelope.To != helper.Name || !strings.Contains(notices[0].Text, "It has no last good listing, so") || !strings.Contains(notices[0].Text, f.task.ID) {
				t.Fatalf("six slow ticks with no last good host listing posted %d notices, want one to the owner helper %s: %+v", len(notices), helper.ID, notices)
			}
		})
	}
}

// r5 (wi_98cbb984acd23d81): a listing that alternates between timing out and
// answering stays in one slow episode and posts one notice. Only three good
// ticks in a row end it, so a later timeout posts a second notice.
func TestTeamRunnerFlappingListingNoticesOncePerSustainedEpisode(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	hub := newSlowListingHub(t, f)
	now := time.Now()
	r := queueCorrectionRunner(t, f, 1)
	r.now = func() time.Time { return now }
	_, _, helper := slowRunningTeam(t, f, hub, r)
	tick := func(what string, slow bool) {
		t.Helper()
		hub.slowActive.Store(slow)
		now = now.Add(2 * queueListingRetryMax)
		before, _ := hub.listings()
		if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if after, _ := hub.listings(); after-before != 1 {
			t.Fatalf("%s made %d active listing calls, want one live attempt", what, after-before)
		}
	}
	for flap := 1; flap <= 5; flap++ {
		tick(fmt.Sprintf("flap %d, slow", flap), true)
		tick(fmt.Sprintf("flap %d, first good tick", flap), false)
		if flap%2 == 0 {
			// Two good ticks in a row are not a sustained recovery either.
			tick(fmt.Sprintf("flap %d, second good tick", flap), false)
		}
		if n := len(slowListingNotices(t, f)); n != 1 {
			t.Fatalf("after flap %d there are %d slow listing notices, want one", flap, n)
		}
	}
	tick("a slow tick after the flaps", true)
	for i := 1; i <= queueListingRecoverTicks; i++ {
		tick(fmt.Sprintf("good tick %d of the recovery", i), false)
	}
	if n := len(slowListingNotices(t, f)); n != 1 {
		t.Fatalf("%d slow listing notices after the sustained recovery, want still one", n)
	}
	tick("a timeout after the sustained recovery", true)
	notices := slowListingNotices(t, f)
	if len(notices) != 2 || notices[1].To != helper.ID || notices[0].Seq == notices[1].Seq {
		t.Fatalf("a timeout after three good ticks: %d notices, want a second one to the owner helper: %+v", len(notices), notices)
	}
}

// r5 (wi_7a64eec43384d77d): on a tick served from the last good host listing
// there is no host census, so a parallel team left launching by a spawn the
// project agent cap refused is not spawned again. The next live tick takes the census first and
// then continues the launch.
func TestTeamRunnerCachedHostListingHoldsParallelLaunch(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	hub := newSlowListingHub(t, f)
	domain, err := canonicalLimiterDomain(hub.c.Base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cached-policy", Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: domain, HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20}); err != nil {
		t.Fatal(err)
	}
	free := int64(1 << 20)
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cached-usage", Operation: "observe_host", Host: "fixture", HostUsage: &api.TeamHostUsage{Host: "fixture", LimiterDomain: domain, PolicyVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 2, Complete: true, SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: &free}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cached-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 2}); err != nil {
		t.Fatal(err)
	}
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cached-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{"src/cached"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var events []string
	failSpawn := true
	r := queueCorrectionRunner(t, f, 1)
	r.now = func() time.Time { return now }
	r.census = func(context.Context, *api.Client, string, string, api.TeamHostPolicy, *api.TeamHostUsage, []string) error {
		events = append(events, "census")
		return nil
	}
	spawn := r.spawn
	r.spawn = func(e env, args []string) error {
		events = append(events, "spawn")
		if failSpawn {
			return fmt.Errorf("register agent: %w", &api.HTTPError{Status: 409, Msg: api.ErrLimit.Error()})
		}
		return spawn(e, args)
	}
	state := func() string {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}
	// A live tick claims the entry and its first spawn is refused at the
	// agent cap: the entry is left launching, and both listings are saved.
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err == nil || !strings.Contains(err.Error(), projectAgentCapPrefix) {
		t.Fatalf("first tick = %v, want the agent cap refusal", err)
	}
	if got := strings.Join(events, ","); got != "census,spawn" || state() != "launching" {
		t.Fatalf("first tick: events %s, entry %s; want census,spawn and a launching entry", got, state())
	}
	// A second live tick lists the entry as launching, so the saved listings
	// name it in that state, and its spawn is refused again.
	events = nil
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err == nil || state() != "launching" {
		t.Fatalf("second tick = %v, entry %s", err, state())
	}
	if got := strings.Join(events, ","); got != "census,spawn" {
		t.Fatalf("second tick: events %s, want census,spawn", got)
	}

	// The host listing stops answering: this tick works from the cached one.
	events, failSpawn = nil, false
	hub.slowHost.Store(true)
	now = now.Add(time.Second)
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatalf("tick on a cached host listing: %v", err)
	}
	if got := strings.Join(events, ","); got != "" || state() != "launching" {
		t.Fatalf("tick on a cached host listing: events %q, entry %s; want no census, no spawn and a launching entry", got, state())
	}
	// Still none on a later tick of the episode that tries the listing again.
	now = now.Add(queueListingRetryFirst)
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatalf("second tick on a cached host listing: %v", err)
	}
	if got := strings.Join(events, ","); got != "" || state() != "launching" {
		t.Fatalf("second tick on a cached host listing: events %q, entry %s; want no census and no spawn", got, state())
	}

	// A live host listing: the census runs, then the launch continues.
	hub.slowHost.Store(false)
	now = now.Add(queueListingRetryMax)
	if err := r.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
		t.Fatalf("live tick after the episode: %v", err)
	}
	if got := strings.Join(events, ","); got != "census,spawn" || state() != "running" {
		t.Fatalf("live tick after the episode: events %s, entry %s; want census,spawn and a running entry", got, state())
	}
}

// wi_38d72bebdaa36497 q2 (wi_c055a34662f66fb6 f3): a serial launching team
// needs no host census, so it continues its spawns on a cached listing of any
// age. A timed-out host listing serves the tick both listings from cache; the
// tick reads the entry again by itself and the launch continues from that
// read. The launch's own project listing still has to answer.
func TestTeamRunnerSerialLaunchContinuesOnOldCachedListing(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	hub := newSlowListingHub(t, f)
	if limit, err := f.c.ListTeamQueue(ctx, f.task.ID); err != nil || queueParallel(limit.ConcurrencyLimit) {
		t.Fatalf("the fixture project is not serial: limit %d, %v", limit.ConcurrencyLimit, err)
	}
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "serial-cached-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	entryPath := "/v1/tasks/" + f.task.ID + "/team-queue/" + q.ID
	var mu sync.Mutex
	entryReads := 0
	counted := *hub.c
	transport := *hub.c.HTTP
	transport.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet && req.URL.Path == entryPath {
			mu.Lock()
			entryReads++
			mu.Unlock()
		}
		return hub.RoundTrip(req)
	})
	counted.HTTP = &transport
	reads := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := entryReads
		entryReads = 0
		return n
	}
	now := time.Now()
	var events []string
	failSpawn := true
	r := queueCorrectionRunner(t, f, 1)
	r.now = func() time.Time { return now }
	r.census = func(context.Context, *api.Client, string, string, api.TeamHostPolicy, *api.TeamHostUsage, []string) error {
		events = append(events, "census")
		return nil
	}
	spawn := r.spawn
	r.spawn = func(e env, args []string) error {
		events = append(events, "spawn")
		if failSpawn {
			return fmt.Errorf("register agent: %w", &api.HTTPError{Status: 409, Msg: api.ErrLimit.Error()})
		}
		return spawn(e, args)
	}
	state := func() string {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}
	// A live tick claims the entry and its first spawn is refused at the
	// agent cap, so it is left launching. A second live tick lists it
	// launching, which both saved listings then name. Live ticks act on the
	// listing and do not read the entry by itself.
	for _, what := range []string{"first live tick", "second live tick"} {
		events = nil
		if err := r.tick(ctx, hub.e, &counted, "fixture"); err == nil || !strings.Contains(err.Error(), projectAgentCapPrefix) {
			t.Fatalf("%s = %v, want the agent cap refusal", what, err)
		}
		if got := strings.Join(events, ","); got != "spawn" || state() != "launching" {
			t.Fatalf("%s: events %s, entry %s; want spawn and a launching entry", what, got, state())
		}
	}
	if n := reads(); n != 0 {
		t.Fatalf("live ticks read the entry by itself %d times, want none", n)
	}

	// Thirty minutes later the host listing stops answering, so the tick
	// works from both listings saved then, older than the ten minutes base
	// allowed. Its one project listing request is the launch's own, which
	// answers.
	events, failSpawn = nil, false
	hub.slowHost.Store(true)
	activeBefore, hostBefore := hub.listings()
	now = now.Add(30 * time.Minute)
	if err := r.tick(ctx, hub.e, &counted, "fixture"); err != nil {
		t.Fatalf("tick on a 30 minute old cached listing: %v", err)
	}
	if active, host := hub.listings(); active-activeBefore != 1 || host-hostBefore != 1 {
		t.Fatalf("the tick made %d project and %d host listing calls, want the launch's own and one slow host attempt", active-activeBefore, host-hostBefore)
	}
	if n := reads(); n != 1 {
		t.Fatalf("the tick read the launching entry by itself %d times, want once", n)
	}
	if got := strings.Join(events, ","); got != "spawn" || state() != "running" {
		t.Fatalf("tick on a 30 minute old cached listing: events %q, entry %s; want no census, one spawn and a running entry", got, state())
	}
	if n := len(slowListingNotices(t, f)); n > 1 {
		t.Fatalf("%d slow listing notices, want at most one", n)
	}
}

// requestLog records the requests a client sends, as "METHOD path" with the
// body of a POST appended.
type requestLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *requestLog) RoundTrip(r *http.Request) (*http.Response, error) {
	line := r.Method + " " + r.URL.Path
	if r.Body != nil && r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		line += " " + string(body)
	}
	l.mu.Lock()
	l.seen = append(l.seen, line)
	l.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

func (l *requestLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.seen
	l.seen = nil
	return out
}

// r2 (wi_3755e4b181d0d590): in a serial project a queue head the token budget
// holds does not take the pass. The runner spends no request on it and claims
// the next entry, which fits; the head stays queued. The store decides both:
// its listed reason for the head starts with the runner's prefix.
func TestTeamRunnerSerialPassesBudgetHeldHead(t *testing.T) {
	f := newTeamFixture(t, true)
	relayState := os.Getenv("TAILTERM_RELAY_STATE")
	capture := captureHome(t)
	t.Setenv("TAILTERM_RELAY_STATE", relayState)
	ctx := context.Background()
	by := api.Caller{Node: "team-fixture", User: "owner"}
	estimate := func(item api.WorkItem, tokens int64) {
		t.Helper()
		basis := "synthetic"
		if _, _, err := f.st.CreateWorkItemUpdate(ctx, f.task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, RequestID: "serial-budget-estimate-" + item.ID, EstimateTokens: &tokens, EstimateBasis: &basis}, by); err != nil {
			t.Fatal(err)
		}
	}
	// The head is a large second item; the fixture's own item, which the
	// runner's plan launches, is queued behind it later.
	big, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "large", RequestID: "serial-budget-large"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "large bounded order", RequestID: "serial-budget-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: big.ID, ItemRevision: big.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.confirmOrder(t, big, order.Seq)
	head, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "serial-budget-head", Operation: "add", ItemID: big.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	estimate(big, 600_000)
	if _, err := f.c.SetUsageBudget(ctx, f.task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	// Half the window is used: 500K remain, which does not cover 600K.
	at := time.Now().UTC()
	writeCapture(t, capture, captureJSON(at.Unix(), 50, at.Add(2*time.Hour).Unix()))
	var reading providerUsageState
	if err := providerUsageTick(ctx, f.c, "fixture", at, &reading); err != nil {
		t.Fatal(err)
	}
	if limit, err := f.c.ListTeamQueue(ctx, f.task.ID); err != nil || queueParallel(limit.ConcurrencyLimit) {
		t.Fatalf("the fixture project is not serial: limit %d, %v", limit.ConcurrencyLimit, err)
	}
	log := &requestLog{}
	counted := *f.c
	transport := *f.c.HTTP
	transport.Transport = log
	counted.HTTP = &transport
	spawns := 0
	r := queueCorrectionRunner(t, f, 1)
	spawn := r.spawn
	r.spawn = func(e env, args []string) error {
		spawns++
		return spawn(e, args)
	}
	state := func(id string) string {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}

	// The held head alone. On base 40fc6ef this tick cost six requests: the
	// host listing, the active listing, and for the head a task read, a
	// listing, an item read and a claim. Now the head costs none.
	const baseHeldHeadTick = 6
	if err := r.tick(ctx, f.e, &counted, "fixture"); err != nil {
		t.Fatal(err)
	}
	alone := log.take()
	for _, line := range alone {
		if strings.Contains(line, big.ID) || strings.Contains(line, `"claim"`) {
			t.Fatalf("the tick spent a request on the budget-held head: %s", line)
		}
	}
	if len(alone) > baseHeldHeadTick-4 || spawns != 0 || state(head.ID) != "queued" {
		t.Fatalf("a tick with only the budget-held head made %d requests (base %d, want at most %d), %d spawns, head %s:\n%s", len(alone), baseHeldHeadTick, baseHeldHeadTick-4, spawns, state(head.ID), strings.Join(alone, "\n"))
	}

	// A second entry that fits is queued behind it.
	later, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "serial-budget-later", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	estimate(f.item, 100_000)
	list, err := f.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil || len(list.Entries) != 2 || list.Entries[0].ID != head.ID || list.Entries[1].ID != later.ID {
		t.Fatalf("active listing %+v %v", list.Entries, err)
	}
	if !strings.HasPrefix(list.Entries[0].BlockReason, tokenBudgetPrefix) || !strings.Contains(list.Entries[0].BlockReason, "needs about 600.00K tokens; 500.00K remain") {
		t.Fatalf("the head's listed reason %q does not start with the runner's prefix %q", list.Entries[0].BlockReason, tokenBudgetPrefix)
	}
	if strings.HasPrefix(list.Entries[1].BlockReason, tokenBudgetPrefix) {
		t.Fatalf("the second entry is budget-held too: %q", list.Entries[1].BlockReason)
	}
	log.take()
	if err := r.tick(ctx, f.e, &counted, "fixture"); err != nil {
		t.Fatal(err)
	}
	claims := 0
	for _, line := range log.take() {
		if strings.Contains(line, big.ID) || strings.Contains(line, `"queue-claim-`+head.ID+`"`) {
			t.Fatalf("the tick spent a request on the budget-held head: %s", line)
		}
		if strings.Contains(line, `"queue-claim-`+later.ID+`"`) {
			claims++
		}
	}
	if claims != 1 || spawns != 1 || state(later.ID) != "running" || state(head.ID) != "queued" {
		t.Fatalf("one tick: %d claims of the second entry, %d spawns, second %s, head %s; want the second entry claimed and running past a queued head", claims, spawns, state(later.ID), state(head.ID))
	}
}

// wi_38d72bebdaa36497 q1: in a serial project a queue head the token budget
// holds is still failed once its item is done or dismissed, as on base, where
// advance read the head's item every tick. The pass still gives it no turn.
func TestTeamRunnerSerialFailsBudgetHeldHeadOfEndedItem(t *testing.T) {
	for _, status := range []string{"done", "dismissed"} {
		t.Run(status, func(t *testing.T) {
			f := newTeamFixture(t, true)
			relayState := os.Getenv("TAILTERM_RELAY_STATE")
			capture := captureHome(t)
			t.Setenv("TAILTERM_RELAY_STATE", relayState)
			ctx := context.Background()
			by := api.Caller{Node: "team-fixture", User: "owner"}
			big, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "large", RequestID: "ended-budget-large"})
			if err != nil {
				t.Fatal(err)
			}
			order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "large bounded order", RequestID: "ended-budget-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: big.ID, ItemRevision: big.Revision, Relationship: "primary"}}})
			if err != nil {
				t.Fatal(err)
			}
			f.confirmOrder(t, big, order.Seq)
			head, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "ended-budget-head", Operation: "add", ItemID: big.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			tokens, basis := int64(600_000), "synthetic"
			if _, _, err := f.st.CreateWorkItemUpdate(ctx, f.task.ID, big.ID, api.CreateWorkItemUpdate{ExpectedRevision: big.Revision, RequestID: "ended-budget-estimate", EstimateTokens: &tokens, EstimateBasis: &basis}, by); err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.SetUsageBudget(ctx, f.task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 1_000_000}); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			writeCapture(t, capture, captureJSON(at.Unix(), 50, at.Add(2*time.Hour).Unix()))
			var reading providerUsageState
			if err := providerUsageTick(ctx, f.c, "fixture", at, &reading); err != nil {
				t.Fatal(err)
			}
			listed := func() string {
				t.Helper()
				list, err := f.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
				if err != nil || len(list.Entries) != 1 || list.Entries[0].ID != head.ID {
					t.Fatalf("active listing %+v %v", list.Entries, err)
				}
				return list.Entries[0].BlockReason
			}
			if reason := listed(); !strings.HasPrefix(reason, tokenBudgetPrefix) {
				t.Fatalf("the open head's listed reason %q does not start with the runner's prefix %q", reason, tokenBudgetPrefix)
			}
			current, err := f.c.GetWorkItem(ctx, f.task.ID, big.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, big.ID, api.UpdateWorkItemRequest{Revision: current.Revision, Status: &status}, by); err != nil {
				t.Fatal(err)
			}
			// The status change is a new item revision, and a terminal item is
			// never rebound, so the hub lists the head as awaiting a rebind
			// instead of by the budget: the pass's budget skip does not apply.
			if reason := listed(); strings.HasPrefix(reason, tokenBudgetPrefix) || !strings.Contains(reason, "tt team queue rebind") {
				t.Fatalf("the %s head's listed reason is %q, want the rebind reason", status, reason)
			}
			if err := (teamRunner{}).tick(ctx, f.e, f.c, "fixture"); err == nil || !strings.Contains(err.Error(), "queued item changed before launch") {
				t.Fatalf("tick = %v, want the head failed as a changed item", err)
			}
			got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, head.ID)
			if err != nil || got.State != "failed" || got.Failure != "queued item changed before launch" {
				t.Fatalf("budget-held head of a %s item: %+v %v", status, got, err)
			}
		})
	}
}

// wi_577abce4b40df47b a3: the real hub lists a finished entry only as
// history. The active view and the host listing never name it, so closeout
// reads the default view, whose history page carries it as a summary. The
// cleanup fixture hub (fixtureQueueView) mirrors this contract.
func TestTeamRunnerCloseoutListingShape(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "closeout-shape-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r := queueCorrectionRunner(t, f, 1)
	entry := func() api.TeamQueueEntry {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for i := 0; i < 2; i++ {
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if got := entry(); got.State != "running" {
		t.Fatalf("entry %s, want running", got.State)
	}
	terminal := "dismissed"
	if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3 && entry().State != "finished"; i++ {
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if got := entry(); got.State != "finished" || got.Host != "fixture" {
		t.Fatalf("entry %s on %q, want finished on fixture", got.State, got.Host)
	}
	names := func(list api.TeamQueueList) (bool, api.TeamQueueEntry) {
		for _, e := range list.Entries {
			if e.ID == q.ID {
				return true, e
			}
		}
		return false, api.TeamQueueEntry{}
	}
	active, err := f.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := names(active); ok {
		t.Fatal("the active view lists a finished entry")
	}
	host, err := f.c.TeamQueueByHost(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := names(host); ok {
		t.Fatal("the host listing lists a finished entry")
	}
	recent, err := f.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ok, got := names(recent)
	if !ok || !got.Summary || got.State != "finished" || got.Host != "fixture" || recent.History == nil || recent.History.Total != 1 || recent.History.Limit != api.DefaultTeamQueueHistoryLimit {
		t.Fatalf("default view %+v: want the finished entry as a history summary", recent)
	}
}

// wi_014682840daf2d94: a queued entry whose item was amended after it was
// queued waits for tt team queue rebind. The runner leaves it queued, sends
// no claim for it and, in a serial project as in a parallel one, claims the
// next entry in the same tick.
func TestTeamRunnerPassesHeadAwaitingRebind(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		name := "serial"
		if parallel {
			name = "parallel"
		}
		t.Run(name, func(t *testing.T) {
			f := newTeamFixture(t, true)
			ctx := context.Background()
			add := func(id string, item api.WorkItem, order int64, owns string) api.TeamQueueEntry {
				t.Helper()
				req := api.TeamQueueRequest{RequestID: "rebind-add-" + id, Operation: "add", ItemID: item.ID, OrderMessageSeq: order, Host: "fixture", Cwd: t.TempDir()}
				if parallel {
					req.Repository, req.BaseCommit, req.Ownership = "fixture-repo", strings.Repeat("a", 40), []string{owns}
				}
				q, err := f.c.TeamQueueAction(ctx, f.task.ID, req)
				if err != nil {
					t.Fatal(err)
				}
				return q
			}
			if parallel {
				domain, err := canonicalLimiterDomain(f.c.Base)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "rebind-policy", Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: domain, HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "rebind-usage", Operation: "observe_host", Host: "fixture", HostUsage: &api.TeamHostUsage{Host: "fixture", LimiterDomain: domain, PolicyVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 2, Complete: true, SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: freeDiskMiB(1 << 20)}}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "rebind-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 2}); err != nil {
					t.Fatal(err)
				}
			}
			// The head is a second item, amended once queued; the fixture's
			// own item, which the runner's plan launches, is queued behind it.
			amended, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "amended", RequestID: "rebind-amended"})
			if err != nil {
				t.Fatal(err)
			}
			order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "amended bounded order", RequestID: "rebind-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: amended.ID, ItemRevision: amended.Revision, Relationship: "primary"}}})
			if err != nil {
				t.Fatal(err)
			}
			f.confirmOrder(t, amended, order.Seq)
			head := add("head", amended, order.Seq, "src/amended")
			later := add("later", f.item, f.order, "src/later")
			desc := "amended after it was queued"
			bumped, err := f.st.UpdateWorkItem(ctx, f.task.ID, amended.ID, api.UpdateWorkItemRequest{Revision: amended.Revision, Description: &desc}, api.Caller{Node: "fixture", User: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			if bumped.Revision <= head.ItemRevision {
				t.Fatalf("the amendment left the item at revision %d, entry at %d", bumped.Revision, head.ItemRevision)
			}
			list, err := f.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
			if err != nil || len(list.Entries) != 2 || list.Entries[0].ID != head.ID || list.Entries[1].ID != later.ID || queueParallel(list.ConcurrencyLimit) != parallel {
				t.Fatalf("active listing %+v limit %d %v", list.Entries, list.ConcurrencyLimit, err)
			}

			log := &requestLog{}
			counted := *f.c
			transport := *f.c.HTTP
			transport.Transport = log
			counted.HTTP = &transport
			spawns := 0
			r := queueCorrectionRunner(t, f, 1)
			r.census = func(context.Context, *api.Client, string, string, api.TeamHostPolicy, *api.TeamHostUsage, []string) error {
				return nil
			}
			spawn := r.spawn
			r.spawn = func(e env, args []string) error {
				spawns++
				return spawn(e, args)
			}
			if err := r.tick(ctx, f.e, &counted, "fixture"); err != nil {
				t.Fatalf("tick = %v; an entry awaiting a rebind is not a failure", err)
			}
			claims := 0
			for _, line := range log.take() {
				if strings.Contains(line, `"queue-claim-`+head.ID+`"`) {
					t.Fatalf("the tick sent a claim for the entry awaiting a rebind: %s", line)
				}
				if strings.Contains(line, `"queue-claim-`+later.ID+`"`) {
					claims++
				}
			}
			gotHead, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, head.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gotHead.State != "queued" || gotHead.EscalationSeq != 0 || gotHead.ItemRevision != head.ItemRevision {
				t.Fatalf("the entry awaiting a rebind is %s (escalation %d, item revision %d); want it left queued at %d", gotHead.State, gotHead.EscalationSeq, gotHead.ItemRevision, head.ItemRevision)
			}
			gotLater, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, later.ID)
			if err != nil {
				t.Fatal(err)
			}
			if claims != 1 || spawns != 1 || gotLater.State != "running" {
				t.Fatalf("one tick: %d claims of the next entry, %d spawns, next %s; want it claimed and running past the entry awaiting a rebind", claims, spawns, gotLater.State)
			}
		})
	}
}

// pendingCleanup is a scripted member cleanup: while pending holds, it
// returns err without a receipt; afterwards it saves the exact run's receipt.
type pendingCleanup struct {
	pending atomic.Bool
	err     error
	calls   atomic.Int32
}

func (p *pendingCleanup) cleanup(f teamFixture) func(context.Context, env, string, string) error {
	return func(ctx context.Context, _ env, task, id string) error {
		p.calls.Add(1)
		if p.pending.Load() {
			return p.err
		}
		a, err := f.c.GetAgent(ctx, task, id)
		if err != nil {
			return err
		}
		_, err = f.c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: a.RunID})
		return err
	}
}

// runningDismissedEntry launches one fake lead for the fixture item, then
// dismisses the item so the next tick closes the team and cleans it up.
func runningDismissedEntry(t *testing.T, f teamFixture, r teamRunner) api.TeamQueueEntry {
	t.Helper()
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "pending-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	terminal := "dismissed"
	if _, err := f.st.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &terminal}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestCloseoutCleanupRetryableErrors(t *testing.T) {
	for _, err := range []error{
		errors.New(cleanupReceiptPending),
		errors.New(cleanupSessionPresent),
		errors.Join(errors.New(cleanupReceiptPending), errors.New(cleanupSessionPresent)),
	} {
		if !cleanupRetryable(err) {
			t.Errorf("%q not retryable", err)
		}
	}
	for _, err := range []error{
		nil,
		errors.Join(),
		errors.New("Agent is not closed"),
		errors.New("Hub unavailable; local session retained"),
		errors.New("Unverified session left open"),
		errors.New("tmux could not stop the session"),
		fmt.Errorf("wrapped: %w", errors.New(cleanupReceiptPending)),
		errors.Join(errors.New(cleanupReceiptPending), errors.New("Agent is not closed")),
	} {
		if cleanupRetryable(err) {
			t.Errorf("%v retryable", err)
		}
	}
}

// A non-retryable member cleanup error still fails the entry, alone or among
// pending ones.
func TestTeamRunnerCloseoutNonRetryableCleanupFails(t *testing.T) {
	for name, cause := range map[string]error{
		"not-closed": errors.New("Agent is not closed"),
		"mixed":      errors.Join(errors.New(cleanupReceiptPending), errors.New("Agent is not closed")),
	} {
		t.Run(name, func(t *testing.T) {
			f := newTeamFixture(t, true)
			ctx := context.Background()
			cleanup := &pendingCleanup{err: cause}
			cleanup.pending.Store(true)
			r := queueCorrectionRunner(t, f, 1)
			r.cleanup = cleanup.cleanup(f)
			r.cleanupPending = &cleanupPendingBook{}
			q := runningDismissedEntry(t, f, r)
			if err := r.tick(ctx, f.e, f.c, "fixture"); err == nil || !strings.Contains(err.Error(), "Agent is not closed") {
				t.Fatalf("tick error %v", err)
			}
			got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
			if err != nil || got.State != "failed" || !strings.Contains(got.Failure, "cleanup: ") || !strings.Contains(got.Failure, "Agent is not closed") {
				t.Fatalf("entry %+v %v", got, err)
			}
		})
	}
}

// Incident I-39544: a member cleanup receipt that is still pending leaves the
// accepted entry running under its frozen close request, and the entry
// finishes with its saved acceptance once the exact run reads cleaned up.
func TestTeamRunnerCloseoutPendingCleanupThenReceiptFinishes(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	base, commit := strings.Repeat("a", 40), strings.Repeat("b", 40)
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "pending-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: base, Ownership: []string{"src/a"}})
	if err != nil {
		t.Fatal(err)
	}
	cleanup := &pendingCleanup{err: errors.New(cleanupReceiptPending)}
	cleanup.pending.Store(true)
	r := queueCorrectionRunner(t, f, 1)
	r.cleanup = cleanup.cleanup(f)
	r.cleanupPending = &cleanupPendingBook{}
	r.integration = func(_ context.Context, q api.TeamQueueEntry, _ api.WorkItem, _ api.TeamCloseRequest) (*api.TeamIntegrationReady, error) {
		a := q.Acceptance
		return &api.TeamIntegrationReady{Repository: a.Repository, BaseCommit: a.BaseCommit, Worktree: a.Worktree, Branch: a.Branch, Commit: a.Commit, Evidence: a.Evidence}, nil
	}
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || q.State != "running" {
		t.Fatalf("launch %+v %v", q, err)
	}
	// The item is done and the handler saves exact Git acceptance.
	by := api.Caller{Node: "fixture", User: "owner"}
	current, err := f.st.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := testverification.Prepare(f.c, f.task.ID, current, commit, q.Repository, q.BaseCommit); err != nil {
		t.Fatal(err)
	}
	report, _, err := f.st.PutNarrativeReport(ctx, f.task.ID, current.ID, api.PutNarrativeReportRequest{RequestID: "pending-report", ScopeRevision: current.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver it.", DeliveredWork: "Synthetic delivery complete.", Verification: "Isolated runner fixture.", Limitations: "Fixture only.", RemainingWork: "Owner integration."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: f.task.ID, ItemID: current.ID, Revision: current.Revision, Label: "bounded scope"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	terminal := "done"
	if _, _, err := f.st.CreateWorkItemUpdate(ctx, f.task.ID, current.ID, api.CreateWorkItemUpdate{ExpectedRevision: current.Revision, RequestID: "pending-done", Status: &terminal, CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by); err != nil {
		t.Fatal(err)
	}
	if current, err = f.c.GetWorkItem(ctx, f.task.ID, current.ID); err != nil {
		t.Fatal(err)
	}
	accepted, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "pending-accept", Operation: "accept", EntryID: q.ID, ExpectedRevision: q.Revision, HandlerAgentID: q.HandlerID, HandlerRunID: q.HandlerRunID, Acceptance: &api.TeamIntegrationAcceptance{Repository: q.Repository, BaseCommit: q.BaseCommit, Worktree: q.Cwd, Branch: "fix/pending", Commit: commit, ItemRevision: current.Revision, CompletionReport: current.CompletionReport, Evidence: "saved fixture acceptance"}})
	if err != nil || accepted.Acceptance == nil {
		t.Fatalf("accept %+v %v", accepted, err)
	}
	time.Sleep(2 * time.Second) // isolated test hub's write bucket refills
	// Two ticks with the receipt still pending: the team closes, the entry
	// stays running with no failure write, and cleanup is tried each tick.
	var frozen string
	for i := 1; i <= 2; i++ {
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatalf("pending tick %d: %v", i, err)
		}
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil || got.State != "running" || got.Failure != "" || got.EscalationSeq != 0 || len(got.CloseJSON) == 0 {
			t.Fatalf("pending tick %d entry %+v %v", i, got, err)
		}
		if i == 1 {
			frozen = string(got.CloseJSON)
		} else if string(got.CloseJSON) != frozen {
			t.Fatalf("close request changed: %s then %s", frozen, got.CloseJSON)
		}
		if n := cleanup.calls.Load(); n != int32(i) {
			t.Fatalf("pending tick %d made %d cleanup calls", i, n)
		}
	}
	var closeReq api.TeamCloseRequest
	if err := json.Unmarshal([]byte(frozen), &closeReq); err != nil || closeReq.RequestID == "" {
		t.Fatalf("frozen close %s %v", frozen, err)
	}
	receipt, err := f.c.GetTeamCloseReceipt(ctx, f.task.ID, closeReq.RequestID)
	if err != nil || len(receipt.Members) != 1 {
		t.Fatalf("close receipt %+v %v", receipt, err)
	}
	member := receipt.Members[0]
	if a, err := f.c.GetAgent(ctx, f.task.ID, member.AgentID); err != nil || a.RunID != member.RunID || a.Status != api.AgentClosed || a.CleanupDone {
		t.Fatalf("member before receipt %+v %v", a, err)
	}
	// The receipt arrives: the next tick cleans up and finishes.
	cleanup.pending.Store(false)
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	if a, err := f.c.GetAgent(ctx, f.task.ID, member.AgentID); err != nil || a.RunID != member.RunID || !a.CleanupDone {
		t.Fatalf("member after receipt %+v %v", a, err)
	}
	got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || got.State != "finished" || got.Failure != "" || got.EscalationSeq != 0 || string(got.CloseJSON) != frozen {
		t.Fatalf("finished entry %+v %v", got, err)
	}
	if got.Acceptance == nil || got.Acceptance.Commit != commit || got.Acceptance.AcceptedAt != accepted.Acceptance.AcceptedAt || got.Integration == nil || got.Integration.Commit != commit || got.Integration.Branch != "fix/pending" {
		t.Fatalf("acceptance or integration lost: %+v %+v", got.Acceptance, got.Integration)
	}
	if len(r.cleanupPending.entries) != 0 {
		t.Fatalf("pending state kept after finish: %v", r.cleanupPending.entries)
	}
}

// A pending cleanup that persists is bounded: one owner-helper notice after
// cleanupPendingNoticeAfter on the runner clock, naming the member and run,
// and never a failure. The notice's request identity dedupes a relay restart.
func TestTeamRunnerCloseoutPersistentPendingCleanupNoticesOnce(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	if _, err := f.st.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "fixture", Session: "owner", Runtime: "claude", RequestID: "pending-helper"}, api.Caller{Node: "team-fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	cleanup := &pendingCleanup{err: errors.New(cleanupSessionPresent)}
	cleanup.pending.Store(true)
	start := time.Now()
	now := start
	r := queueCorrectionRunner(t, f, 1)
	r.cleanup = cleanup.cleanup(f)
	r.cleanupPending = &cleanupPendingBook{}
	r.now = func() time.Time { return now }
	q := runningDismissedEntry(t, f, r)
	notices := func() []api.Message {
		t.Helper()
		messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 200)
		if err != nil {
			t.Fatal(err)
		}
		var out []api.Message
		for _, m := range messages {
			if m.Envelope != nil && m.Envelope.Subject == cleanupPendingTitle {
				out = append(out, m)
			}
		}
		return out
	}
	tick := func(at time.Duration, wantNotices int) {
		t.Helper()
		now = start.Add(at)
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatalf("tick at %s: %v", at, err)
		}
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil || got.State != "running" || got.Failure != "" || got.EscalationSeq != 0 {
			t.Fatalf("tick at %s entry %+v %v", at, got, err)
		}
		if n := len(notices()); n != wantNotices {
			t.Fatalf("tick at %s: %d notices, want %d", at, n, wantNotices)
		}
	}
	tick(0, 0)
	tick(29*time.Minute, 0)
	tick(30*time.Minute, 1)
	tick(45*time.Minute, 1)
	// A relay restart forgets the start and times a fresh interval; its
	// notice replays the saved one.
	r.cleanupPending = &cleanupPendingBook{}
	tick(50*time.Minute, 1)
	tick(80*time.Minute, 1)
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lead, helper api.Agent
	for _, a := range detail.Agents {
		switch {
		case a.Name == "correction-lead-0":
			lead = a
		case a.Role == api.AgentRoleOwnerHelper:
			helper = a
		}
	}
	notice := notices()[0]
	if notice.To != helper.ID || notice.Envelope.Kind != api.EnvelopeKindNotice || notice.Envelope.Refs["member"] != lead.ID || notice.Envelope.Refs["run"] != lead.RunID || notice.Envelope.Refs["entry"] != q.ID ||
		!strings.Contains(notice.Text, lead.ID) || !strings.Contains(notice.Text, lead.RunID) || !strings.Contains(notice.Text, "30m0s") {
		t.Fatalf("notice %+v to %s, lead %s/%s", notice.Envelope, notice.To, lead.ID, lead.RunID)
	}
	if lead.CleanupDone {
		t.Fatal("pending cleanup reads done")
	}
	if n := cleanup.calls.Load(); n != 6 {
		t.Fatalf("%d cleanup calls, want one per tick", n)
	}
}

// refusingNoticeTransport answers the relay's cleanup pending notice with
// status while it is set, and passes every other request to the fake hub.
type refusingNoticeTransport struct {
	status atomic.Int32
	posts  atomic.Int32
}

func (t *refusingNoticeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/messages") && req.Header.Get(relayAuthorHeader) != "" && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		if strings.Contains(string(body), cleanupPendingTitle) {
			t.posts.Add(1)
			if status := int(t.status.Load()); status != 0 {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"fixture refusal"}`)), Request: req}, nil
			}
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// wi_46e8a874f780be0c: a notice the hub refuses with a non-409 error backs
// off on the runner clock, doubling from cleanupNoticeRetryFirst, and logs
// once per try instead of every tick; a 409 counts as posted.
func TestTeamRunnerCloseoutRefusedPendingNoticeBacksOff(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	if _, err := f.st.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "fixture", Session: "owner", Runtime: "claude", RequestID: "refused-helper"}, api.Caller{Node: "team-fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	refuse := &refusingNoticeTransport{}
	f.c.HTTP.Transport = refuse
	cleanup := &pendingCleanup{err: errors.New(cleanupSessionPresent)}
	cleanup.pending.Store(true)
	start := time.Now()
	now := start
	r := queueCorrectionRunner(t, f, 1)
	r.cleanup = cleanup.cleanup(f)
	r.cleanupPending = &cleanupPendingBook{}
	r.now = func() time.Time { return now }
	q := runningDismissedEntry(t, f, r)
	refuse.status.Store(http.StatusServiceUnavailable)
	var log strings.Builder
	tick := func(at time.Duration, wantPosts int32) {
		t.Helper()
		now = start.Add(at)
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatalf("tick at %s: %v", at, err)
		}
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil || got.State != "running" || got.Failure != "" {
			t.Fatalf("tick at %s entry %+v %v", at, got, err)
		}
		if n := refuse.posts.Load(); n != wantPosts {
			t.Fatalf("tick at %s: %d notice posts, want %d", at, n, wantPosts)
		}
	}
	restore := captureStderr(t, &log)
	tick(0, 0)
	tick(30*time.Minute, 1)
	tick(30*time.Minute+30*time.Second, 1)
	tick(31*time.Minute, 2)
	tick(32*time.Minute, 2)
	tick(33*time.Minute, 3)
	tick(36*time.Minute, 3)
	tick(37*time.Minute, 4)
	restore()
	if n := strings.Count(log.String(), "cleanup pending notice: "); n != 4 {
		t.Fatalf("%d notice log lines, want one per refused post:\n%s", n, log.String())
	}
	for _, next := range []string{"next try in 1m0s", "next try in 2m0s", "next try in 4m0s", "next try in 8m0s"} {
		if !strings.Contains(log.String(), next) {
			t.Fatalf("log lacks %q:\n%s", next, log.String())
		}
	}
	// The delay stops doubling at cleanupNoticeRetryMax.
	if len(r.cleanupPending.entries) != 1 {
		t.Fatalf("book %v", r.cleanupPending.entries)
	}
	var key string
	for key = range r.cleanupPending.entries {
	}
	for i := 0; i < 10; i++ {
		r.cleanupPending.refused(key, now)
	}
	if got := r.cleanupPending.entries[key].next.Sub(now); got != cleanupNoticeRetryMax {
		t.Fatalf("capped delay %s, want %s", got, cleanupNoticeRetryMax)
	}
	r.cleanupPending.entries[key].next = start.Add(45 * time.Minute)
	// A 409 is the hub's answer to a replayed identity: posted, never tried again.
	refuse.status.Store(http.StatusConflict)
	tick(45*time.Minute, 5)
	tick(80*time.Minute, 5)
	if s := r.cleanupPending.entries[key]; s == nil || !s.noticed {
		t.Fatalf("409 not counted as posted: %+v", s)
	}
}

// wi_46e8a874f780be0c: a pending cleanup key goes as soon as its entry leaves
// closeout by any path, so the relay's book cannot grow until a restart.
func TestTeamRunnerCloseoutPendingKeysLeaveWithTheirEntry(t *testing.T) {
	ctx := context.Background()
	t.Run("failed-on-a-later-member", func(t *testing.T) {
		f := newTeamFixture(t, true)
		r := queueCorrectionRunner(t, f, 2)
		r.cleanupPending = &cleanupPendingBook{}
		var calls atomic.Int32
		var waiting int
		r.cleanup = func(context.Context, env, string, string) error {
			if calls.Add(1) == 1 {
				return errors.New(cleanupReceiptPending)
			}
			waiting = len(r.cleanupPending.entries)
			return errors.New("Agent is not closed")
		}
		q := runningDismissedEntry(t, f, r)
		if err := r.tick(ctx, f.e, f.c, "fixture"); err == nil || !strings.Contains(err.Error(), "Agent is not closed") {
			t.Fatalf("tick error %v", err)
		}
		if got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || got.State != "failed" {
			t.Fatalf("entry %+v %v", got, err)
		}
		if waiting != 1 || len(r.cleanupPending.entries) != 0 {
			t.Fatalf("first member key %d before the failure, %v after", waiting, r.cleanupPending.entries)
		}
	})
	t.Run("failed-elsewhere-or-unlisted", func(t *testing.T) {
		f := newTeamFixture(t, true)
		cleanup := &pendingCleanup{err: errors.New(cleanupSessionPresent)}
		cleanup.pending.Store(true)
		r := queueCorrectionRunner(t, f, 1)
		r.cleanup = cleanup.cleanup(f)
		r.cleanupPending = &cleanupPendingBook{}
		q := runningDismissedEntry(t, f, r)
		// An entry no listing names, another hub's entry and the live one.
		gone := cleanupPendingKey(f.c.Base, "tqe_gone", "agt_gone", "run_gone")
		otherHub := cleanupPendingKey("http://other-hub", q.ID, "agt_other", "run_other")
		r.cleanupPending.wait(gone, time.Now())
		r.cleanupPending.wait(otherHub, time.Now())
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
		if err != nil || got.State != "running" {
			t.Fatalf("pending entry %+v %v", got, err)
		}
		if _, ok := r.cleanupPending.entries[gone]; ok || len(r.cleanupPending.entries) != 2 || r.cleanupPending.entries[otherHub] == nil {
			t.Fatalf("after pending tick %v", r.cleanupPending.entries)
		}
		// Failed by the owner or another relay: the next tick sees it failed.
		if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "elsewhere-fail", Operation: "fail", EntryID: got.ID, ExpectedRevision: got.Revision, Failure: "failed elsewhere"}); err != nil {
			t.Fatal(err)
		}
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		if len(r.cleanupPending.entries) != 1 || r.cleanupPending.entries[otherHub] == nil {
			t.Fatalf("after failed tick %v", r.cleanupPending.entries)
		}
	})
}

// retryFailures is the launch retry book's failure count for key, or -1 when
// it holds no state for it.
func retryFailures(b *launchRetryBook, key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state := b.entries[key]; state != nil {
		return state.failures
	}
	return -1
}

// wi_96690c4ac2d62577 t1: after a pass whose listing of the project is
// complete, the launch retry book holds no state for an entry absent from it
// or listed in a non-launching state, even one the pass never reached. A
// still-launching entry keeps its failures and backoff, and an entry whose
// project listing failed is not dropped.
func TestTeamRunnerLaunchRetryBookForgetsEntriesThatLeaveTheListing(t *testing.T) {
	f, hub, q, runner, now, _ := launchRetryFixture(t)
	ctx := context.Background()
	hub.refuse("freeze", 503, "service unavailable")
	tick := func() { _ = runner.tick(ctx, hub.e, hub.c, "fixture") }
	tick()
	key := launchRetryKey(hub.c.Base, f.task.ID, q.ID)
	if retryFailures(runner.retries, key) != 1 || hub.count("freeze") != 1 {
		t.Fatalf("first launch error not recorded: failures=%d freezes=%d", retryFailures(runner.retries, key), hub.count("freeze"))
	}
	// An entry this project's listing never names, and another hub's key.
	absent := launchRetryKey(hub.c.Base, f.task.ID, "tqe_absent")
	otherHub := launchRetryKey("http://other-hub.invalid", f.task.ID, q.ID)
	runner.retries.record(absent, errors.New("absent"), *now)
	runner.retries.record(otherHub, errors.New("other hub"), *now)

	tick() // inside the backoff
	if retryFailures(runner.retries, key) != 1 || !runner.retries.waiting(key, *now) || hub.count("freeze") != 1 {
		t.Fatalf("still-launching entry lost its backoff: failures=%d waiting=%v freezes=%d", retryFailures(runner.retries, key), runner.retries.waiting(key, *now), hub.count("freeze"))
	}
	if retryFailures(runner.retries, absent) != -1 {
		t.Fatal("an entry absent from a complete listing kept its retry state")
	}

	// A failed project listing observes nothing, so nothing is dropped.
	runner.retries.record(absent, errors.New("absent"), *now)
	hub.setListing(&api.HTTPError{Status: 500, Msg: "listing broke"}, nil)
	tick()
	if retryFailures(runner.retries, absent) != 1 || retryFailures(runner.retries, key) != 1 {
		t.Fatalf("a failed listing dropped retry state: absent=%d launching=%d", retryFailures(runner.retries, absent), retryFailures(runner.retries, key))
	}

	// Listed as running behind a failed head of a serial queue, so the pass
	// never reaches it: still forgotten.
	hub.setListing(nil, func(list *api.TeamQueueList) {
		list.ConcurrencyLimit = 1
		head := api.TeamQueueEntry{ID: "tqe_failedhead", TaskID: f.task.ID, Host: "fixture", State: "failed"}
		for i := range list.Entries {
			if list.Entries[i].ID == q.ID {
				list.Entries[i].State = "running"
			}
		}
		list.Entries = append([]api.TeamQueueEntry{head}, list.Entries...)
	})
	tick()
	if retryFailures(runner.retries, key) != -1 || retryFailures(runner.retries, absent) != -1 {
		t.Fatalf("an entry listed as running kept retry state: entry=%d absent=%d", retryFailures(runner.retries, key), retryFailures(runner.retries, absent))
	}

	// Listed as launching again, the launch starts a fresh count at once.
	hub.setListing(nil, nil)
	tick()
	if retryFailures(runner.retries, key) != 1 || hub.count("freeze") != 2 {
		t.Fatalf("relisted launch did not record a fresh error: failures=%d freezes=%d", retryFailures(runner.retries, key), hub.count("freeze"))
	}
	// Absent from both listings: forgotten on the next pass.
	hub.setListing(nil, func(list *api.TeamQueueList) {
		kept := list.Entries[:0]
		for _, entry := range list.Entries {
			if entry.ID != q.ID {
				kept = append(kept, entry)
			}
		}
		list.Entries = kept
	})
	tick()
	if retryFailures(runner.retries, key) != -1 {
		t.Fatal("an entry absent from the listings kept its retry state")
	}
	if retryFailures(runner.retries, otherHub) != 1 {
		t.Fatal("another hub's retry state was dropped")
	}
}

// wi_96690c4ac2d62577 t2: narrowing runs the changed-file diff once per
// acceptance base and commit across ticks and once more for a new commit,
// and its cache forgets an entry that leaves the listing.
func TestTeamRunnerNarrowDiffsOncePerAcceptanceCommit(t *testing.T) {
	f := newTeamFixture(t, true)
	hub := newLaunchFaultHub(t, f)
	ctx := context.Background()
	base := strings.Repeat("a", 40)
	q, err := hub.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "narrow-cache-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: base, Ownership: []string{"src/a"}})
	if err != nil {
		t.Fatal(err)
	}
	q = runQueueEntry(t, f, q.ID)
	// The listing carries an acceptance; the item stays open, so finish
	// waits and every tick narrows the running entry.
	accept := func(commit string) {
		hub.setListing(nil, func(list *api.TeamQueueList) {
			for i := range list.Entries {
				if list.Entries[i].ID == q.ID {
					list.Entries[i].Acceptance = &api.TeamIntegrationAcceptance{BaseCommit: base, Commit: commit}
				}
			}
		})
	}
	var diffs []string
	runner := teamRunner{changedFiles: &changedFilesBook{}, changed: func(_ context.Context, repository, from, to string) ([]string, error) {
		diffs = append(diffs, from+".."+to)
		return []string{"src/a"}, nil
	}}
	tick := func() {
		t.Helper()
		if err := runner.tick(ctx, hub.e, hub.c, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	first, second := strings.Repeat("b", 40), strings.Repeat("c", 40)
	accept(first)
	for i := 0; i < 3; i++ {
		tick()
	}
	if len(diffs) != 1 || diffs[0] != base+".."+first {
		t.Fatalf("three ticks on one acceptance ran %d diffs: %v", len(diffs), diffs)
	}
	accept(second)
	for i := 0; i < 3; i++ {
		tick()
	}
	if len(diffs) != 2 || diffs[1] != base+".."+second {
		t.Fatalf("a new acceptance commit ran %d diffs in total: %v", len(diffs), diffs)
	}
	if hub.count("scope") != 0 {
		t.Fatalf("unchanged ownership sent %d scope writes", hub.count("scope"))
	}
	if got, err := hub.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || got.State != "running" || got.Revision != q.Revision {
		t.Fatalf("narrowing changed the entry: %+v %v", got, err)
	}
	if len(runner.changedFiles.entries) != 1 {
		t.Fatalf("cache holds %d entries", len(runner.changedFiles.entries))
	}
	hub.setListing(nil, func(list *api.TeamQueueList) { list.Entries = nil })
	tick()
	if len(runner.changedFiles.entries) != 0 {
		t.Fatalf("cache kept %d entries after the entry left the listing", len(runner.changedFiles.entries))
	}
}

// wi_96690c4ac2d62577 t3: one pass reads the project once for all its failed
// unreleased parallel entries, and a local release error that repeats is
// reported once per distinct message, then again when it changes.
func TestTeamRunnerFailedReleaseReadsTaskOncePerPassAndReportsErrorsOnce(t *testing.T) {
	f := newTeamFixture(t, true)
	hub := newLaunchFaultHub(t, f)
	ctx := context.Background()
	choresParallelProject(t, f, 0)
	other, otherOrder := queueFixtureItem(t, f, "release-errors-other")
	var failed []api.TeamQueueEntry
	for i, item := range []struct {
		id    string
		order int64
	}{{f.item.ID, f.order}, {other.ID, otherOrder}} {
		q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("release-errors-add-%d", i), Operation: "add", ItemID: item.id, OrderMessageSeq: item.order, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{fmt.Sprintf("src/%d", i)}})
		if err != nil {
			t.Fatal(err)
		}
		detail, err := f.c.GetTask(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("release-errors-claim-%d", i), Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "fixture", PauseGeneration: detail.Task.PauseGeneration})
		if err != nil {
			t.Fatal(err)
		}
		q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("release-errors-fail-%d", i), Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "fixture failure"})
		if err != nil {
			t.Fatal(err)
		}
		failed = append(failed, q)
	}
	// The host listing names no failed entry, so a queued one brings the
	// project into the pass. The fixture policy's limiter domain differs
	// from the test hub, so it holds.
	queued, queuedOrder := queueFixtureItem(t, f, "release-errors-queued")
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "release-errors-add-queued", Operation: "add", ItemID: queued.ID, OrderMessageSeq: queuedOrder, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{"src/queued"}}); err != nil {
		t.Fatal(err)
	}
	census := func(context.Context, *api.Client, string, string, api.TeamHostPolicy, *api.TeamHostUsage, []string) error {
		t.Fatal("mismatched limiter domain reached census")
		return nil
	}
	// The listed launch journal decides the local release error.
	journal := func(raw string) {
		hub.setListing(nil, func(list *api.TeamQueueList) {
			for i := range list.Entries {
				if list.Entries[i].State == "failed" {
					list.Entries[i].LaunchJSON = json.RawMessage(raw)
				}
			}
		})
	}
	const invalid = "failed queue launch journal is invalid"
	const elsewhere = "uncertain queue release must run on the saved launch host"
	runner := teamRunner{census: census, releaseErrors: &releaseErrorBook{}}
	pass := func(r teamRunner) (string, int) {
		t.Helper()
		reads := hub.count("get_task")
		err := r.tick(ctx, hub.e, hub.c, "fixture")
		text := ""
		if err != nil {
			text = err.Error()
		}
		return text, hub.count("get_task") - reads
	}
	journal(`"not a journal"`)
	for i, want := range []int{2, 0, 0} {
		text, reads := pass(runner)
		if reads != 1 {
			t.Fatalf("pass %d read the task %d times for two failed entries", i, reads)
		}
		if got := strings.Count(text, invalid); got != want {
			t.Fatalf("pass %d reported the repeated error %d times, want %d: %q", i, got, want, text)
		}
	}
	journal(`{"members":[{"state":"uncertain","fields":{"agentId":"agt_0123456789abcdef","name":"uncertain-member"}}]}`)
	for i, want := range []int{2, 0} {
		text, reads := pass(runner)
		if reads != 1 || strings.Count(text, elsewhere) != want || strings.Contains(text, invalid) {
			t.Fatalf("changed error pass %d: reads=%d %q", i, reads, text)
		}
	}
	// Without the book every pass reports it, as before.
	for i := 0; i < 2; i++ {
		if text, _ := pass(teamRunner{census: census}); strings.Count(text, elsewhere) != 2 {
			t.Fatalf("unbooked pass %d: %q", i, text)
		}
	}
	// A valid journal releases both; the book forgets them.
	hub.setListing(nil, nil)
	time.Sleep(2 * time.Second) // isolated test hub's write bucket refills
	if text, reads := pass(runner); text != "" || reads != 1 || hub.count("release") != 2 {
		t.Fatalf("release pass: reads=%d releases=%d %q", reads, hub.count("release"), text)
	}
	for _, q := range failed {
		if got, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || got.ReleasedAt == "" {
			t.Fatalf("entry %s not released: %+v %v", q.ID, got, err)
		}
	}
	if len(runner.releaseErrors.entries) != 0 {
		t.Fatalf("release errors kept %d entries", len(runner.releaseErrors.entries))
	}
}

// wi_ec5791a8d911ca45 e1: narrowing caches only a deterministic Git failure,
// one Git decided by exiting non-zero while the tick's context was live. A
// diff cut short by a cancelled or expired context, or any other transient
// error, is retried on the next pass, and a cached failure is retried when
// the acceptance commit changes. The fake hub sees no call: ownership is
// already narrow.
func TestTeamRunnerNarrowRetriesTransientDiffErrors(t *testing.T) {
	root, base := queueGitRepo(t)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "a"), []byte("a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, root, "add", "src/a")
	gitFixtureCommand(t, root, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-q", "-m", "change")
	good := gitFixtureCommand(t, root, "rev-parse", "HEAD")
	missing := strings.Repeat("f", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("narrowing called the hub: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	diffs := 0
	var fault error  // returned instead of running Git
	var after func() // runs after Git, before narrow classifies its error
	runner := teamRunner{changedFiles: &changedFilesBook{}, changed: func(ctx context.Context, repository, from, to string) ([]string, error) {
		diffs++
		if fault != nil {
			return nil, fault
		}
		files, err := queueChangedFiles(ctx, repository, from, to)
		if after != nil {
			after()
		}
		return files, err
	}}
	q := api.TeamQueueEntry{ID: "tqe_narrow", TaskID: "tsk_narrow", Revision: 1, State: "running", Repository: filepath.Join(root, ".git"), Ownership: []string{"src/a"}}
	key := launchRetryKey(c.Base, q.TaskID, q.ID)
	pass := func(step string, ctx context.Context, commit string, wantDiffs int, wantCached bool) *changedFilesState {
		t.Helper()
		q.Acceptance = &api.TeamIntegrationAcceptance{BaseCommit: base, Commit: commit}
		if got := runner.narrow(ctx, c, q); got.Revision != q.Revision || !sameOwnership(got.Ownership, q.Ownership) {
			t.Fatalf("%s: narrowing changed the entry: %+v", step, got)
		}
		state, cached := runner.changedFiles.lookup(key, base, commit)
		if diffs != wantDiffs || cached != wantCached {
			t.Fatalf("%s: diffs=%d cached=%v, want diffs=%d cached=%v", step, diffs, cached, wantDiffs, wantCached)
		}
		return state
	}
	live := context.Background()
	cancelled, cancel := context.WithCancel(live)
	cancel()
	expired, cancelExpired := context.WithDeadline(live, time.Now().Add(-time.Second))
	defer cancelExpired()
	pass("cancelled context", cancelled, good, 1, false)
	pass("expired context", expired, good, 2, false)
	if state := pass("live retry", live, good, 3, true); state.err != nil || !sameOwnership(state.files, []string{"src/a"}) {
		t.Fatalf("live diff cached %+v", state)
	}
	pass("cached success", live, good, 3, true)
	// Git exits non-zero, but the tick's context ended while it ran.
	ending, end := context.WithCancel(live)
	after = end
	pass("context ended during a failing diff", ending, missing, 4, false)
	after = nil
	fault = errors.New("fork/exec git: resource temporarily unavailable")
	pass("transient start failure", live, missing, 5, false)
	fault = nil
	state := pass("deterministic failure", live, missing, 6, true)
	var exit *exec.ExitError
	if !errors.As(state.err, &exit) || exit.ExitCode() <= 0 {
		t.Fatalf("cached failure is not a Git exit: %v", state.err)
	}
	pass("cached deterministic failure", live, missing, 6, true)
	pass("new acceptance commit", live, good, 7, true)
}

// wi_ec5791a8d911ca45 e2: a transport fault on a hub call inside a failed
// entry's release is a hub fault. It is returned on every pass and never
// muted, while a local error repeating with the same message is still
// returned once per change. The fake hub drops the release's entry fetch.
func TestTeamRunnerFailedReleaseReturnsHubTransportFaultEveryPass(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	var fetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/tasks/tsk_release/team-queue/tqe_release" {
			fetches.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		t.Errorf("unexpected hub call %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	host := spawn.Host()
	const uncertain = `{"members":[{"state":"uncertain","fields":{"agentId":"agt_0123456789abcdef","name":"uncertain-member"}}]}`
	q := api.TeamQueueEntry{ID: "tqe_release", TaskID: "tsk_release", ItemID: "wi_release", Host: host, State: "failed", Revision: 3, LaunchJSON: json.RawMessage(uncertain)}
	task := func() (api.TaskDetail, error) {
		return api.TaskDetail{Task: api.Task{ID: q.TaskID, Status: api.TaskOpen, PauseState: api.ProjectPauseActive}}, nil
	}
	runner := teamRunner{releaseErrors: &releaseErrorBook{}}
	e := env{hub: srv.URL, task: q.TaskID}
	wantHubFault := func(pass string) {
		t.Helper()
		before := fetches.Load()
		err := runner.releaseFailed(ctx, e, c, q, host, task)
		var transport *url.Error
		if err == nil || !errors.As(err, &transport) || !strings.HasPrefix(err.Error(), "hub: ") {
			t.Fatalf("%s: want a hub transport fault, got %v", pass, err)
		}
		if fetches.Load() == before {
			t.Fatalf("%s: the release did not fetch the entry", pass)
		}
		if len(runner.releaseErrors.entries) != 0 {
			t.Fatalf("%s: hub fault left %d release error entries", pass, len(runner.releaseErrors.entries))
		}
	}
	wantHubFault("first pass")
	wantHubFault("second pass")
	q.LaunchJSON = json.RawMessage(`"not a journal"`)
	for i, want := range []bool{true, false, false} {
		err := runner.releaseFailed(ctx, e, c, q, host, task)
		if (err != nil) != want || (err != nil && !strings.Contains(err.Error(), "failed queue launch journal is invalid")) {
			t.Fatalf("local pass %d: %v, want returned=%v", i, err, want)
		}
	}
	// The local error stopped repeating, so the book forgets it.
	q.LaunchJSON = json.RawMessage(uncertain)
	wantHubFault("hub fault after a local error")
}

// wi_5bb3a86587b571ae h1-h3: the host census goes to the first listed project
// that is open. A closed project's owner-integrated failed entry still lists
// on its host, and the hub refuses that project's census, so it must neither
// take the census nor hold the open projects' parallel launches.
func TestTeamRunnerHostCensusSkipsClosedProject(t *testing.T) {
	for _, listing := range []string{"closed-head", "all-closed", "open"} {
		t.Run(listing, func(t *testing.T) { teamRunnerHostCensusSkipsClosedProject(t, listing) })
	}
}

func teamRunnerHostCensusSkipsClosedProject(t *testing.T, listing string) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	// Two projects in host listing order, which is by project ID: the
	// first is closed with an owner-integrated failed entry, the second
	// is open and parallel.
	closed, open := censusProject(t, f, "first"), censusProject(t, f, "second")
	if open.task.ID < closed.task.ID {
		closed, open = open, closed
	}
	domain, err := canonicalLimiterDomain(f.c.Base)
	if err != nil {
		t.Fatal(err)
	}
	free := int64(1 << 20)
	seed := api.TeamHostUsage{Host: "fixture", LimiterDomain: domain, PolicyVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 2, Complete: true, SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: &free}
	for _, req := range []api.TeamQueueRequest{
		{RequestID: "census-policy", Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: domain, HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20},
		{RequestID: "census-usage", Operation: "observe_host", Host: "fixture", HostUsage: &seed},
		{RequestID: "census-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 2},
	} {
		if _, err := f.c.TeamQueueAction(ctx, open.task.ID, req); err != nil {
			t.Fatal(err)
		}
	}
	if listing != "open" {
		q, err := f.c.TeamQueueAction(ctx, closed.task.ID, api.TeamQueueRequest{RequestID: "closed-add", Operation: "add", ItemID: closed.item.ID, OrderMessageSeq: closed.order, Host: "fixture", Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		for _, req := range []api.TeamQueueRequest{
			{RequestID: "closed-claim", Operation: "claim", Host: "fixture"},
			{RequestID: "closed-fail", Operation: "fail", Failure: "fixture"},
			{RequestID: "closed-integrated", Operation: "owner_integrated", OwnerIntegrationCommit: strings.Repeat("b", 40), OwnerIntegrationEvidence: "owner release record"},
		} {
			req.EntryID, req.ExpectedRevision = q.ID, q.Revision
			if q, err = f.c.TeamQueueAction(ctx, closed.task.ID, req); err != nil {
				t.Fatalf("%s: %v", req.Operation, err)
			}
		}
		if _, err := f.c.CloseTask(ctx, closed.task.ID); err != nil {
			t.Fatal(err)
		}
	}
	var entry api.TeamQueueEntry
	if listing != "all-closed" {
		if entry, err = f.c.TeamQueueAction(ctx, open.task.ID, api.TeamQueueRequest{RequestID: "census-add", Operation: "add", ItemID: open.item.ID, OrderMessageSeq: open.order, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{"src/census"}}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := f.c.TeamQueueByHost(ctx, "fixture")
	if err != nil || len(list.Entries) == 0 {
		t.Fatalf("host listing %+v %v", list, err)
	}
	if head := list.Entries[0]; listing != "open" && (head.TaskID != closed.task.ID || head.OwnerIntegration == nil) {
		t.Fatalf("listing head %s %s, want the closed project's owner-integrated entry", head.TaskID, head.State)
	}

	var targets []string
	posts := 0
	r := queueCorrectionRunner(t, open, 1)
	// As in production, a census fault is reported, not returned: the
	// report's stamp is how this test sees a hold.
	r.roundRobin, r.closedProjects = true, &closedProjectBook{}
	r.census = func(ctx context.Context, c *api.Client, task, host string, policy api.TeamHostPolicy, _ *api.TeamHostUsage, _ []string) error {
		targets, posts = append(targets, task), posts+1
		usage := api.TeamHostUsage{Host: host, LimiterDomain: policy.LimiterDomain, PolicyVersion: policy.Version, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 2, Complete: true, SourceDigest: strings.Repeat("c", 64), FreeDiskMiB: &free}
		_, err := c.TeamQueueAction(ctx, task, api.TeamQueueRequest{RequestID: fmt.Sprintf("census-%d", posts), Operation: "observe_host", Host: host, HostUsage: &usage})
		return err
	}
	held := teamHostBudgetLastLog.Swap(0)
	defer teamHostBudgetLastLog.Store(held)
	tick := func(name string, want ...string) {
		t.Helper()
		targets = nil
		err := r.tick(ctx, f.e, f.c, "fixture")
		if err != nil && strings.Contains(err.Error(), "census") {
			t.Fatalf("%s: tick %v", name, err)
		}
		if got := strings.Join(targets, ","); got != strings.Join(want, ",") {
			t.Fatalf("%s: census targets %q, want %q", name, got, strings.Join(want, ","))
		}
		if teamHostBudgetLastLog.Load() != 0 {
			t.Fatalf("%s: the host census held parallel launches", name)
		}
	}
	usage := func() string {
		t.Helper()
		list, err := f.c.TeamQueueByHost(ctx, "fixture")
		if err != nil || list.HostUsage == nil {
			t.Fatalf("host usage %+v %v", list.HostUsage, err)
		}
		return list.HostUsage.SourceDigest
	}
	state := func() string {
		t.Helper()
		got, err := f.c.GetTeamQueueEntry(ctx, open.task.ID, entry.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State
	}
	switch listing {
	case "closed-head":
		// The closed project refuses once, the open one takes the census
		// and its parallel launch proceeds; later ticks skip the closed one.
		tick("first tick", closed.task.ID, open.task.ID)
		if got := state(); got != "running" {
			t.Fatalf("open project's parallel entry %s, want running", got)
		}
		if got := usage(); got != strings.Repeat("c", 64) {
			t.Fatalf("host usage digest %s, want the open project's census", got)
		}
		tick("second tick", open.task.ID)
	case "all-closed":
		// No open project: no census is saved and nothing is held. The
		// refusal is remembered, so the next tick asks no one.
		tick("first tick", closed.task.ID)
		tick("second tick")
		if got := usage(); got != seed.SourceDigest {
			t.Fatalf("host usage digest %s, want the seeded one unchanged", got)
		}
	case "open":
		tick("first tick", open.task.ID)
		if got := state(); got != "running" {
			t.Fatalf("open project's parallel entry %s, want running", got)
		}
		tick("second tick", open.task.ID)
	}
}

// censusProject adds a project with a running handler and one bounded,
// scope-confirmed item. It returns f naming that project, its item and order.
func censusProject(t *testing.T, f teamFixture, name string) teamFixture {
	t.Helper()
	ctx := context.Background()
	task, err := f.c.CreateTask(ctx, api.CreateTaskRequest{Name: name + " census project"})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := f.c.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name + "-handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: name + "-handler", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
	item, err := f.c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: name + " delivery", RequestID: name + "-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "bounded " + name + " order", RequestID: name + "-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: name + "-scope", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	f.task, f.item, f.order, f.handler = task, item, order.Seq, handler
	return f
}
