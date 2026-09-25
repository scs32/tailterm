package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

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
