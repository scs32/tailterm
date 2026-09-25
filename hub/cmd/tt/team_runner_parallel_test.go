package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestParallelRunnerRotatesProjectPollingOrder(t *testing.T) {
	projects := []string{"first", "second", "third"}
	if got := rotateQueueProjects(projects, 1); !reflect.DeepEqual(got, []string{"second", "third", "first"}) {
		t.Fatalf("rotated projects=%v", got)
	}
	if !reflect.DeepEqual(projects, []string{"first", "second", "third"}) {
		t.Fatalf("rotation changed source order: %v", projects)
	}
}

func TestTeamRunnerParallelSkipsConflictAndFinishesOtherSlot(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	otherHandler, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "aux-handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "aux-handler", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: otherHandler.ID, RunID: otherHandler.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
	items := []api.WorkItem{f.item}
	orders := []int64{f.order}
	for i := 1; i < 3; i++ {
		item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: fmt.Sprintf("parallel item %d", i), RequestID: fmt.Sprintf("parallel-item-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		message, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "bounded parallel order", RequestID: fmt.Sprintf("parallel-order-%d", i), WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
		if err != nil {
			t.Fatal(err)
		}
		f.confirmOrder(t, item, message.Seq)
		items = append(items, item)
		orders = append(orders, message.Seq)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "parallel-policy", Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 20, HostMaxPolling: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "parallel-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 2}); err != nil {
		t.Fatal(err)
	}
	var entries []api.TeamQueueEntry
	for i, owned := range []string{"src/a", "src/a/child", "src/c"} {
		entry, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("parallel-add-%d", i), Operation: "add", ItemID: items[i].ID, OrderMessageSeq: orders[i], Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ownership: []string{owned}})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	spawns := map[string]int{}
	runner := teamRunner{
		plan: func(_ context.Context, in map[string]any, out *teamLaunchResolved) error {
			itemID := in["item"].(string)
			for i, item := range items {
				if item.ID == itemID {
					out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, item, api.Message{TaskID: f.task.ID, Seq: orders[i], Text: "bounded parallel order"})
					out.Plan = []teamLaunchEntry{{Fields: teamLaunchFields{Name: "lead-" + itemID[3:11], Role: "lead", Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}}}
					if itemID == items[0].ID {
						out.Plan = append(out.Plan, teamLaunchEntry{Fields: teamLaunchFields{Name: "worker-" + itemID[3:11], Role: "builder", Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}})
					}
					return nil
				}
			}
			return fmt.Errorf("unknown item %s", itemID)
		},
		spawn: func(_ env, args []string) error {
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i += 2 {
				flags[args[i]] = args[i+1]
			}
			data, err := os.ReadFile(flags["--work-context-file"])
			if err != nil {
				return err
			}
			revision, err := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			if err != nil {
				return err
			}
			order, err := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			if err != nil {
				return err
			}
			_, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: flags["--work-item"], ItemRevision: revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: order}, ContextBundle: data}})
			if err != nil {
				return err
			}
			spawns[flags["--work-item"]]++
			return nil
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
		cleanup: func(ctx context.Context, _ env, task, id string) error {
			agent, err := f.c.GetAgent(ctx, task, id)
			if err != nil {
				return err
			}
			_, err = f.c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: agent.RunID})
			return err
		},
		integration: func(_ context.Context, q api.TeamQueueEntry, _ api.WorkItem, closeReq api.TeamCloseRequest) (*api.TeamIntegrationReady, error) {
			return &api.TeamIntegrationReady{Repository: q.Repository, BaseCommit: q.BaseCommit, Branch: "feature/item-c", Commit: strings.Repeat("b", 40), Evidence: "close=" + closeReq.RequestID}, nil
		},
	}
	time.Sleep(2 * time.Second) // isolated test hub's write bucket refills
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"running", "queued", "running"} {
		entry, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, entries[i].ID)
		if err != nil || entry.State != want {
			t.Fatalf("entry %d state=%s want=%s: %v", i, entry.State, want, err)
		}
	}
	if spawns[items[0].ID] != 2 || spawns[items[1].ID] != 0 || spawns[items[2].ID] != 1 {
		t.Fatalf("spawns=%v", spawns)
	}
	a, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, entries[0].ID)
	c, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, entries[2].ID)
	if a.HandlerID == c.HandlerID || a.HandlerRunID == "" || c.HandlerRunID == "" {
		t.Fatalf("handlers not independently leased: A=%+v C=%+v", a, c)
	}
	for _, index := range []int{0, 2} {
		link := []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: items[index].ID, ItemRevision: items[index].Revision, Relationship: "primary"}}
		message, err := f.st.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: fmt.Sprintf("parallel-role-%d", index), WorkItems: link, WorkOrderMessage: &api.MessageReference{TaskID: f.task.ID, Seq: orders[index]}, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: "role:database_handler", Subject: "Check this item", Body: api.EnvelopeBody{Ask: "Check the item."}}}, api.Caller{Node: "fixture", User: "owner"})
		want := a.HandlerID
		if index == 2 {
			want = c.HandlerID
		}
		if err != nil || message.To != want {
			t.Fatalf("item %d handler routing %q want %q: %v", index, message.To, want, err)
		}
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var replacement, leadC api.Agent
	for _, agent := range detail.Agents {
		if agent.WorkItem == nil {
			continue
		}
		if agent.WorkItem.ItemID == items[0].ID && !agent.ItemLead {
			replacement = agent
		}
		if agent.WorkItem.ItemID == items[2].ID && agent.ItemLead {
			leadC = agent
		}
	}
	if replacement.ID == "" || leadC.ID == "" {
		t.Fatalf("missing item leads or replacement: %+v", detail.Agents)
	}
	if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: replacement.ID, RunID: replacement.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
	a, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "parallel-replace-a", Operation: "replace_lead", EntryID: a.ID, ExpectedRevision: a.Revision, LeadAgentID: replacement.ID, LeadRunID: replacement.RunID, ExpectedLeadRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	newLeadA, err := f.c.GetAgent(ctx, f.task.ID, replacement.ID)
	if err != nil || !newLeadA.ItemLead || newLeadA.ItemLeadRevision != 2 {
		t.Fatalf("replacement lead %+v %v", newLeadA, err)
	}
	stillLeadC, err := f.c.GetAgent(ctx, f.task.ID, leadC.ID)
	if err != nil || !stillLeadC.ItemLead || stillLeadC.RunID != leadC.RunID {
		t.Fatalf("C lead changed: %+v %v", stillLeadC, err)
	}
	failed, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "parallel-fail-a", Operation: "fail", EntryID: a.ID, ExpectedRevision: a.Revision, Failure: "synthetic A failure"})
	if err != nil || failed.EscalationSeq == 0 {
		t.Fatalf("fail A: %+v %v", failed, err)
	}
	terminal := "done"
	current, err := f.st.GetWorkItem(ctx, f.task.ID, items[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	by := api.Caller{Node: "fixture", User: "owner"}
	report, _, err := f.st.PutNarrativeReport(ctx, f.task.ID, current.ID, api.PutNarrativeReportRequest{RequestID: "parallel-c-report", ScopeRevision: current.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver C.", DeliveredWork: "Synthetic C delivery complete.", Verification: "Isolated runner fixture.", Limitations: "Fixture only.", RemainingWork: "Owner integration."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: f.task.ID, ItemID: current.ID, Revision: current.Revision, Label: "bounded scope"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.CreateWorkItemUpdate(ctx, f.task.ID, current.ID, api.CreateWorkItemUpdate{ExpectedRevision: current.Revision, RequestID: "parallel-c-done", Status: &terminal, CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by); err != nil {
		t.Fatal(err)
	}
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	c, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, entries[2].ID)
	if err != nil || c.State != "finished" || c.Integration == nil || c.Integration.Branch != "feature/item-c" || c.Integration.Commit != strings.Repeat("b", 40) {
		t.Fatalf("C did not finish independently: %+v %v", c, err)
	}
	b, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, entries[1].ID)
	if err != nil || b.State != "queued" || spawns[items[1].ID] != 0 {
		t.Fatalf("conflicting B crossed A failure: %+v %v", b, err)
	}
	still, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, a.ID)
	if err != nil || still.EscalationSeq != failed.EscalationSeq || still.ReleasedAt != "" {
		t.Fatalf("A reservation/escalation changed: %+v %v", still, err)
	}
}
