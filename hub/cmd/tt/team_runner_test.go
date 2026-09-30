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
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
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
// team queue operations and counts every team queue action by operation.
type launchFaultHub struct {
	e      env
	c      *api.Client
	mu     sync.Mutex
	faults map[string]*api.HTTPError
	counts map[string]int
}

func newLaunchFaultHub(t *testing.T, f teamFixture) *launchFaultHub {
	t.Helper()
	h := &launchFaultHub{faults: map[string]*api.HTTPError{}, counts: map[string]int{}}
	inner := server.New(f.st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "team-fixture", User: "owner"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
