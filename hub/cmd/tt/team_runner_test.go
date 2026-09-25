package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
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
