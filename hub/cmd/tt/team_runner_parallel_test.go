package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/testverification"
)

func TestParallelRunnerHostEvidenceFailureKeepsSerialProjectMoving(t *testing.T) {
	for _, failure := range []string{"census", "domain"} {
		t.Run(failure, func(t *testing.T) {
			f := newTeamFixture(t, true)
			ctx := context.Background()
			parallelTask, err := f.c.CreateTask(ctx, api.CreateTaskRequest{Name: "parallel peer"})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := f.c.AddAgent(ctx, parallelTask.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "peer-handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "peer-handler", Runtime: "codex"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.PostEvent(ctx, parallelTask.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventRunning}); err != nil {
				t.Fatal(err)
			}
			item, err := f.c.CreateWorkItem(ctx, parallelTask.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "peer delivery", RequestID: "peer-item"})
			if err != nil {
				t.Fatal(err)
			}
			order, err := f.c.PostMessage(ctx, parallelTask.ID, api.PostMessageRequest{Text: "bounded peer order", RequestID: "peer-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: parallelTask.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.ConfirmWorkOrderScope(ctx, parallelTask.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "peer-scope", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
				t.Fatal(err)
			}
			domain, err := canonicalLimiterDomain(f.c.Base)
			if err != nil {
				t.Fatal(err)
			}
			policy := api.TeamQueueRequest{RequestID: "peer-policy", Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: domain, HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20}
			if _, err := f.c.TeamQueueAction(ctx, parallelTask.ID, policy); err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.TeamQueueAction(ctx, parallelTask.ID, api.TeamQueueRequest{RequestID: "peer-usage", Operation: "observe_host", Host: "fixture", HostUsage: &api.TeamHostUsage{Host: "fixture", LimiterDomain: domain, PolicyVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 2, Complete: true, SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: freeDiskMiB(1 << 20)}}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.TeamQueueAction(ctx, parallelTask.ID, api.TeamQueueRequest{RequestID: "peer-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 2}); err != nil {
				t.Fatal(err)
			}
			serial, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "serial-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			parallel, err := f.c.TeamQueueAction(ctx, parallelTask.ID, api.TeamQueueRequest{RequestID: "peer-add", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{"src/peer"}})
			if err != nil {
				t.Fatal(err)
			}
			runner := queueCorrectionRunner(t, f, 1)
			if failure == "census" {
				runner.census = func(ctx context.Context, c *api.Client, task, host string, policy api.TeamHostPolicy, _ *api.TeamHostUsage, _ []string) error {
					domain, version := policy.LimiterDomain, policy.Version
					_, err := c.TeamQueueAction(ctx, task, api.TeamQueueRequest{RequestID: "future-census", Operation: "observe_host", Host: host, HostUsage: &api.TeamHostUsage{Host: host, LimiterDomain: domain, PolicyVersion: version, ObservedAt: time.Now().UTC().Add(7 * time.Second).Format(time.RFC3339Nano), RelayBindings: 2, Complete: true, SourceDigest: strings.Repeat("b", 64)}})
					if err == nil {
						t.Fatal("test hub accepted a clock-skewed census")
					}
					return err
				}
			} else {
				policy.RequestID, policy.HostPolicyVersion, policy.LimiterDomain = "peer-policy-mismatch", 2, "https://other.invalid"
				if _, err := f.c.TeamQueueAction(ctx, parallelTask.ID, policy); err != nil {
					t.Fatal(err)
				}
				runner.census = func(context.Context, *api.Client, string, string, api.TeamHostPolicy, *api.TeamHostUsage, []string) error {
					t.Fatal("mismatched domain reached census")
					return nil
				}
			}
			if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
				t.Fatalf("host evidence failure reached global queue backoff: %v", err)
			}
			serial, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, serial.ID)
			if err != nil || serial.State != "running" {
				t.Fatalf("unrelated serial project did not advance: %+v %v", serial, err)
			}
			parallel, err = f.c.GetTeamQueueEntry(ctx, parallelTask.ID, parallel.ID)
			if err != nil || parallel.State != "queued" {
				t.Fatalf("unsafe parallel launch advanced: %+v %v", parallel, err)
			}
		})
	}
}

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
	for _, limit := range []int{2, 0} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) { teamRunnerParallelSkipsConflictAndFinishesOtherSlot(t, limit) })
	}
}

func teamRunnerParallelSkipsConflictAndFinishesOtherSlot(t *testing.T, limit int) {
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
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "parallel-policy", Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 20, HostMaxPolling: 2, LimiterDomain: "https://fixture.invalid", HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "parallel-usage", Operation: "observe_host", Host: "fixture", HostUsage: &api.TeamHostUsage{Host: "fixture", LimiterDomain: "https://fixture.invalid", PolicyVersion: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 1, Complete: true, SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: freeDiskMiB(1 << 20)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "parallel-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: limit}); err != nil {
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
		integration: func(_ context.Context, q api.TeamQueueEntry, _ api.WorkItem, _ api.TeamCloseRequest) (*api.TeamIntegrationReady, error) {
			a := q.Acceptance
			return &api.TeamIntegrationReady{Repository: a.Repository, BaseCommit: a.BaseCommit, Worktree: a.Worktree, Branch: a.Branch, Commit: a.Commit, Evidence: a.Evidence}, nil
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
	missingReceipt := api.TeamQueueRequest{RequestID: "parallel-c-missing-receipt", Operation: "accept", EntryID: c.ID, ExpectedRevision: c.Revision, HandlerAgentID: c.HandlerID, HandlerRunID: c.HandlerRunID, Acceptance: &api.TeamIntegrationAcceptance{Repository: c.Repository, BaseCommit: c.BaseCommit, Commit: strings.Repeat("b", 40), Worktree: c.Cwd, Branch: "feature/item-c", ItemRevision: current.Revision, Evidence: "fixture"}}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, missingReceipt); err == nil {
		t.Fatal("queue accepted without receipt")
	}
	if err := testverification.Prepare(f.c, f.task.ID, current, strings.Repeat("b", 40), c.Repository, c.BaseCommit); err != nil {
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
	current, err = f.c.GetWorkItem(ctx, f.task.ID, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	waiting, err := f.c.ListTeamQueue(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var waitingEntry api.TeamQueueEntry
	for _, entry := range waiting.Entries {
		if entry.ID == entries[2].ID {
			waitingEntry = entry
		}
	}
	if waitingEntry.State != "running" || waitingEntry.BlockReason != "Waiting for handler acceptance" {
		t.Fatalf("missing acceptance did not visibly hold the slot: %+v", waitingEntry)
	}
	queueOutput, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil || !strings.Contains(queueOutput, "Waiting for handler acceptance") {
		t.Fatalf("CLI did not show acceptance wait: %q %v", queueOutput, err)
	}
	c, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, entries[2].ID)
	if err != nil {
		t.Fatal(err)
	}
	acceptReq := api.TeamQueueRequest{RequestID: "parallel-c-accept", Operation: "accept", EntryID: c.ID, ExpectedRevision: c.Revision, HandlerAgentID: c.HandlerID, HandlerRunID: c.HandlerRunID, Acceptance: &api.TeamIntegrationAcceptance{Repository: c.Repository, BaseCommit: c.BaseCommit, Worktree: c.Cwd, Branch: "feature/item-c", Commit: strings.Repeat("b", 40), ItemRevision: current.Revision, CompletionReport: current.CompletionReport, Evidence: "saved fixture acceptance"}}
	accepted, err := f.c.TeamQueueAction(ctx, f.task.ID, acceptReq)
	if err != nil {
		t.Fatal(err)
	}
	acceptReq.ExpectedRevision = accepted.Revision // Lost-response replay uses the same request identity.
	if replay, err := f.c.TeamQueueAction(ctx, f.task.ID, acceptReq); err != nil || replay.Acceptance == nil || replay.Acceptance.AcceptedAt != accepted.Acceptance.AcceptedAt {
		t.Fatalf("acceptance receipt replay %+v %v", replay, err)
	}
	acceptReq.Acceptance.Commit = strings.Repeat("c", 40)
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, acceptReq); err == nil {
		t.Fatal("conflicting accepted SHA reused the receipt identity")
	}
	// Public verification setup uses the same bounded write bucket; refill it
	// before testing the unrelated limiter-domain contract.
	time.Sleep(2 * time.Second)
	// A policy/domain mismatch must not stop an already-running item's
	// owner-gated close and integration path.
	runner.census = func(context.Context, *api.Client, string, string, api.TeamHostPolicy, *api.TeamHostUsage, []string) error {
		t.Fatal("mismatched limiter domain reached census")
		return nil
	}
	if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatalf("domain mismatch reached global queue backoff: %v", err)
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

func freeDiskMiB(n int64) *int64 { return &n }

// releaseCounter counts team queue release requests sent through a client.
type releaseCounter struct {
	next     http.RoundTripper
	releases atomic.Int64
	actions  atomic.Int64
}

func (r *releaseCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/team-queue/actions") && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		r.actions.Add(1)
		if bytes.Contains(body, []byte(`"operation":"release"`)) {
			r.releases.Add(1)
		}
	}
	return r.next.RoundTrip(req)
}

func observeRunnerHost(t *testing.T, f teamFixture, host string, version int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("policy-%s-%d", host, version), Operation: "set_host_policy", Host: host, HostPolicyVersion: version, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: "https://fixture.invalid", HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: fmt.Sprintf("usage-%s-%d", host, version), Operation: "observe_host", Host: host, HostUsage: &api.TeamHostUsage{Host: host, LimiterDomain: "https://fixture.invalid", PolicyVersion: version, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), RelayBindings: 1, Complete: true, SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: freeDiskMiB(1 << 20)}}); err != nil {
		t.Fatal(err)
	}
}

// a8: in a parallel project the runner releases a failed entry once its
// item-bound runs are closed and cleaned, and never while one is live; a
// serial project still waits for the owner.
func TestTeamRunnerReleasesFailedParallelEntry(t *testing.T) {
	for _, limit := range []int{0, 1} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			f := newTeamFixture(t, true)
			ctx := context.Background()
			observeRunnerHost(t, f, "fixture", 1)
			if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "release-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: limit}); err != nil {
				t.Fatal(err)
			}
			add := func(item api.WorkItem, order int64, owns string) api.TeamQueueEntry {
				q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add-" + item.ID, Operation: "add", ItemID: item.ID, OrderMessageSeq: order, Host: "fixture", Cwd: t.TempDir(), Repository: "fixture-repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{owns}})
				if err != nil {
					t.Fatal(err)
				}
				return q
			}
			a := add(f.item, f.order, "src/a")
			other, otherOrder := queueFixtureItem(t, f, "release-other")
			c := add(other, otherOrder, "src/c")
			detail, err := f.c.GetTask(ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			a, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "claim-a", Operation: "claim", EntryID: a.ID, ExpectedRevision: a.Revision, Host: "fixture", PauseGeneration: detail.Task.PauseGeneration})
			if err != nil {
				t.Fatal(err)
			}
			member, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "member-a", Host: "fixture", Session: "member-a", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order}, ContextBundle: teamCloseCLIContext(t, f.item, api.Message{TaskID: f.task.ID, Seq: f.order, Text: "bounded fixture order"})}})
			if err != nil {
				t.Fatal(err)
			}
			failed, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "fail-a", Operation: "fail", EntryID: a.ID, ExpectedRevision: a.Revision, Failure: "owner integrated abc1234"})
			if err != nil {
				t.Fatal(err)
			}
			counter := &releaseCounter{next: http.DefaultTransport}
			counted := *f.c
			counted.HTTP = &http.Client{Timeout: 10 * time.Second, Transport: counter}
			// The fixture policy's limiter domain differs from the test hub,
			// so queued entries hold and only the failed entry can advance.
			runner := teamRunner{
				plan: func(context.Context, map[string]any, *teamLaunchResolved) error {
					t.Fatal("runner launched a queued entry")
					return nil
				},
				census: func(context.Context, *api.Client, string, string, api.TeamHostPolicy, *api.TeamHostUsage, []string) error {
					t.Fatal("mismatched limiter domain reached census")
					return nil
				},
			}
			for i := 0; i < 2; i++ {
				if err := runner.tick(ctx, f.e, &counted, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			if counter.actions.Load() != 0 {
				t.Fatalf("live failed entry cost %d queue writes", counter.actions.Load())
			}
			list, err := f.c.ListTeamQueue(ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := list.Entries[0]; got.ReleasedAt != "" || got.Revision != failed.Revision {
				t.Fatalf("live failed entry changed: %+v", got)
			} else if limit == 0 && got.BlockReason != "Failed; 1 item-bound runs are still live or uncleaned" {
				t.Fatalf("live-run reason %q", got.BlockReason)
			}
			if _, err := f.c.CloseAgent(ctx, f.task.ID, member.ID, member.RunID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.ReportCleanup(ctx, f.task.ID, member.ID, api.CleanupRequest{RunID: member.RunID}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Second) // isolated test hub's write bucket refills
			if err := runner.tick(ctx, f.e, &counted, "fixture"); err != nil {
				t.Fatal(err)
			}
			released, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if limit == 1 {
				if released.ReleasedAt != "" || counter.releases.Load() != 0 {
					t.Fatalf("serial failed entry was released automatically: %+v", released)
				}
				return
			}
			if released.ReleasedAt == "" || released.State != "failed" || released.Failure != failed.Failure || released.EscalationSeq != failed.EscalationSeq || counter.releases.Load() != 1 {
				t.Fatalf("closed failed entry not released once: %+v releases=%d", released, counter.releases.Load())
			}
			observeRunnerHost(t, f, "fixture", 2)
			detail, err = f.c.GetTask(ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "claim-c", Operation: "claim", EntryID: c.ID, ExpectedRevision: c.Revision, Host: "fixture", PauseGeneration: detail.Task.PauseGeneration})
			if err != nil || claimed.HandlerID != a.HandlerID || claimed.HandlerLeaseGeneration <= a.HandlerLeaseGeneration {
				t.Fatalf("next claim did not take the freed handler: %+v %v", claimed, err)
			}
		})
	}
}

// a3 (runner side): the census reports the least free space across the
// host's queue worktrees and writes early when the reserve comparison flips.
func TestHostCensusReportsLeastFreeDisk(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	free := map[string]int64{a: 9000, b: 3000}
	statfs := func(path string) (int64, error) {
		if v, ok := free[path]; ok {
			return v, nil
		}
		return 0, os.ErrNotExist
	}
	if got := hostFreeDiskMiB([]string{a, b, a, filepath.Join(a, "gone")}, statfs); got == nil || *got != 3000 {
		t.Fatalf("least free disk %v", got)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	free[wd] = 7000
	if got := hostFreeDiskMiB([]string{filepath.Join(a, "gone")}, statfs); got == nil || *got != 7000 {
		t.Fatalf("fallback free disk %v", got)
	}
	delete(free, wd)
	if got := hostFreeDiskMiB(nil, statfs); got != nil {
		t.Fatalf("unobservable disk reported %d", *got)
	}

	f := newTeamFixture(t, true)
	ctx := context.Background()
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "census-policy", Operation: "set_host_policy", Host: "fixture", HostPolicyVersion: 1, HostPolicyExpires: time.Now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: "https://fixture.invalid", HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20, HostMinFreeDiskMiB: 1000}); err != nil {
		t.Fatal(err)
	}
	previous := statfsFreeMiB
	t.Cleanup(func() { statfsFreeMiB = previous })
	census := func(value int64) *api.TeamHostUsage {
		t.Helper()
		statfsFreeMiB = func(string) (int64, error) { return value, nil }
		list, err := f.c.TeamQueueByHost(ctx, "fixture")
		if err != nil || list.HostPolicy == nil {
			t.Fatalf("host list %+v %v", list, err)
		}
		if err := saveHostRelayCensus(ctx, f.c, f.task.ID, "fixture", *list.HostPolicy, list.HostUsage, []string{a, b}, time.Now()); err != nil {
			t.Fatal(err)
		}
		list, err = f.c.TeamQueueByHost(ctx, "fixture")
		if err != nil || list.HostUsage == nil {
			t.Fatalf("host usage %+v %v", list, err)
		}
		return list.HostUsage
	}
	if got := census(5000); got.FreeDiskMiB == nil || *got.FreeDiskMiB != 5000 {
		t.Fatalf("first census %+v", got)
	}
	if got := census(4000); got.FreeDiskMiB == nil || *got.FreeDiskMiB != 5000 {
		t.Fatalf("unchanged reserve state rewrote the census: %+v", got)
	}
	if got := census(500); got.FreeDiskMiB == nil || *got.FreeDiskMiB != 500 {
		t.Fatalf("reserve flip did not write the census: %+v", got)
	}
}
