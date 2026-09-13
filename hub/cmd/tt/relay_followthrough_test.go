package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func followThroughTestBinding(hub string) runtimeBinding {
	return runtimeBinding{Hub: hub, Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-4000-8000-000000000001", Codex: "/usr/local/bin/codex"}
}

func followThroughTestDelivery(b runtimeBinding) api.RequiredDelivery {
	return api.RequiredDelivery{ID: "dly_0000000000000001", TaskID: b.Task, AgentID: b.Agent, RunID: b.Run, ItemTaskID: b.Task, ItemID: "wi_0000000000000001", ItemRevision: 1, Generation: 2, ExecutionEpoch: 3, Current: true, EnrollmentVersion: api.ReliableFollowThroughCapabilityVersion, FollowThrough: api.DeliveryFollowThroughState{AttemptCount: 0}}
}

func TestRelayFollowThroughSkipsBlockedSiblingAndQueuesExecutableAction(t *testing.T) {
	b := followThroughTestBinding("")
	blocked := followThroughTestDelivery(b)
	blocked.ID, blocked.Generation, blocked.RecipientKind, blocked.ActionKey, blocked.ActionClass = "dly_0000000000000002", 2, api.DeliveryRecipientProjectLead, "release-planned-wait", api.DeliveryActionExecution
	blocked.EnrollmentVersion = api.ReliableMandatoryActionCapabilityVersion
	executable := followThroughTestDelivery(b)
	executable.ID, executable.Generation, executable.RecipientKind, executable.ActionKey, executable.ActionClass = "dly_0000000000000003", 3, api.DeliveryRecipientProjectLead, "dispatch-independent", api.DeliveryActionIndependentDispatch
	executable.EnrollmentVersion = api.ReliableMandatoryActionCapabilityVersion
	var checked []string
	var queueCalls int
	var leaseID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/capabilities":
			json.NewEncoder(w).Encode(api.CurrentCapabilities())
		case strings.HasSuffix(r.URL.Path, "/delivery-coverages"):
			json.NewEncoder(w).Encode(api.DeliveryCoverageList{TaskID: b.Task, AgentID: b.Agent, RunID: b.Run, Actions: []api.DeliveryCoverage{
				{Status: "covered", AgentStatus: api.AgentRunning, Delivery: &blocked},
				{Status: "covered", AgentStatus: api.AgentRunning, Delivery: &executable},
			}})
		case strings.HasSuffix(r.URL.Path, "/follow-through/check"):
			var check api.DeliveryFollowThroughCheckRequest
			json.NewDecoder(r.Body).Decode(&check)
			checked = append(checked, r.URL.Path)
			if strings.Contains(r.URL.Path, blocked.ID) {
				json.NewEncoder(w).Encode(api.DeliveryFollowThroughDecision{Delivery: blocked, Classification: api.DeliveryFollowThroughKnownBlock})
				return
			}
			leaseID = check.RequestID
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughDecision{Delivery: executable, Classification: api.DeliveryFollowThroughOverdueUnknown, Action: api.DeliveryFollowThroughActionQueue, Attempt: 1, Execute: true})
		case strings.HasSuffix(r.URL.Path, "/delivery-coverage"):
			if r.URL.Query().Get("itemId") != executable.ItemID || r.URL.Query().Get("actionKey") != executable.ActionKey {
				t.Fatalf("revalidation did not select exact executable action: %s", r.URL.RawQuery)
			}
			fresh := executable
			fresh.FollowThrough.PendingAction = api.DeliveryFollowThroughActionQueue
			fresh.FollowThrough.PendingRequestID = leaseID
			json.NewEncoder(w).Encode(api.DeliveryCoverage{Status: "covered", AgentStatus: api.AgentRunning, Delivery: &fresh})
		case strings.HasSuffix(r.URL.Path, "/follow-through/report"):
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughReport{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	c, err := api.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := relayFollowThrough(context.Background(), b, &relayProgress{}, c, time.Now().UTC(), func(_ context.Context, _ runtimeBinding, prompt string) error {
		queueCalls++
		if !strings.Contains(prompt, "--item "+executable.ItemID+" --action-key "+executable.ActionKey) || !strings.Contains(prompt, "cannot satisfy a sibling action") {
			t.Fatalf("prompt omitted exact sibling responsibility: %q", prompt)
		}
		return nil
	})
	if err != nil || !queued || queueCalls != 1 || len(checked) != 2 || !strings.Contains(checked[0], blocked.ID) || !strings.Contains(checked[1], executable.ID) {
		t.Fatalf("blocked sibling prevented independent dispatch: queued=%v queueCalls=%d checked=%v err=%v", queued, queueCalls, checked, err)
	}
}

func TestRelayFollowThroughRevalidatesAndRetriesOnlyDurableReport(t *testing.T) {
	var coverageCalls, checkCalls, reportCalls, queueCalls int
	var leaseRequestID string
	var reported api.DeliveryFollowThroughReportRequest
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/capabilities":
			json.NewEncoder(w).Encode(api.CurrentCapabilities())
		case strings.HasSuffix(r.URL.Path, "/delivery-coverage"):
			coverageCalls++
			b := followThroughTestBinding(server.URL)
			d := followThroughTestDelivery(b)
			if coverageCalls > 1 {
				d.FollowThrough.PendingAction = api.DeliveryFollowThroughActionQueue
				d.FollowThrough.PendingRequestID = leaseRequestID
			}
			json.NewEncoder(w).Encode(api.DeliveryCoverage{TaskID: b.Task, AgentID: b.Agent, RunID: b.Run, Status: "covered", Delivery: &d})
		case strings.HasSuffix(r.URL.Path, "/follow-through/check"):
			checkCalls++
			var check api.DeliveryFollowThroughCheckRequest
			json.NewDecoder(r.Body).Decode(&check)
			leaseRequestID = check.RequestID
			b := followThroughTestBinding(server.URL)
			d := followThroughTestDelivery(b)
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughDecision{Delivery: d, Classification: api.DeliveryFollowThroughOverdueUnknown, Action: api.DeliveryFollowThroughActionQueue, Attempt: 1, Execute: true})
		case strings.HasSuffix(r.URL.Path, "/follow-through/report"):
			reportCalls++
			if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
				t.Fatal(err)
			}
			if reportCalls == 1 {
				http.Error(w, "synthetic response loss", http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughReport{Replay: reportCalls > 2})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b := followThroughTestBinding(server.URL)
	c, err := api.NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p := relayProgress{}
	queue := func(_ context.Context, got runtimeBinding, prompt string) error {
		queueCalls++
		if got.Run != b.Run || !strings.Contains(prompt, "delivery dly_0000000000000001 generation 2 epoch 3") || !strings.Contains(prompt, "Before starting another tool") {
			t.Fatalf("unsafe follow-through prompt: %+v %q", got, prompt)
		}
		return nil
	}
	now := time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	queued, err := relayFollowThrough(context.Background(), b, &p, c, now, queue)
	if err == nil || !queued || queueCalls != 1 || p.PendingFollowThroughReport == nil || reported.Outcome != api.DeliveryFollowThroughOutcomeAccepted {
		t.Fatalf("first response-loss pass: queued=%v calls=%d pending=%+v reported=%+v err=%v", queued, queueCalls, p.PendingFollowThroughReport, reported, err)
	}
	// The next pass retries only the stable report. The fake check still says
	// execute, so stop after the report by making coverage explicitly uncovered.
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/capabilities":
			json.NewEncoder(w).Encode(api.CurrentCapabilities())
		case strings.HasSuffix(r.URL.Path, "/follow-through/report"):
			reportCalls++
			var retry api.DeliveryFollowThroughReportRequest
			json.NewDecoder(r.Body).Decode(&retry)
			if retry != reported {
				t.Fatalf("report retry changed identity/payload: %+v vs %+v", retry, reported)
			}
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughReport{Replay: true})
		case strings.HasSuffix(r.URL.Path, "/delivery-coverage"):
			json.NewEncoder(w).Encode(api.DeliveryCoverage{TaskID: b.Task, AgentID: b.Agent, RunID: b.Run, Status: api.DeliveryFollowThroughUncovered})
		default:
			http.NotFound(w, r)
		}
	})
	queued, err = relayFollowThrough(context.Background(), b, &p, c, now.Add(time.Second), queue)
	if err != nil || queued || queueCalls != 1 || p.PendingFollowThroughReport != nil || reportCalls != 2 {
		t.Fatalf("stable report retry repeated external queue: queued=%v queue=%d reports=%d pending=%+v err=%v", queued, queueCalls, reportCalls, p.PendingFollowThroughReport, err)
	}
	if coverageCalls != 2 || checkCalls != 1 {
		t.Fatalf("expected initial coverage + pre-dispatch revalidation: coverage=%d check=%d", coverageCalls, checkCalls)
	}
}

func TestRelayFollowThroughStalePreDispatchCoverageNeverQueues(t *testing.T) {
	var coverageCalls, queueCalls int
	var outcome string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b := followThroughTestBinding("")
		d := followThroughTestDelivery(b)
		switch {
		case r.URL.Path == "/v1/capabilities":
			json.NewEncoder(w).Encode(api.CurrentCapabilities())
		case strings.HasSuffix(r.URL.Path, "/delivery-coverage"):
			coverageCalls++
			if coverageCalls == 1 {
				json.NewEncoder(w).Encode(api.DeliveryCoverage{Status: "covered", Delivery: &d})
			} else {
				json.NewEncoder(w).Encode(api.DeliveryCoverage{Status: api.DeliveryFollowThroughUncovered})
			}
		case strings.HasSuffix(r.URL.Path, "/follow-through/check"):
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughDecision{Delivery: d, Action: api.DeliveryFollowThroughActionQueue, Execute: true})
		case strings.HasSuffix(r.URL.Path, "/follow-through/report"):
			var report api.DeliveryFollowThroughReportRequest
			json.NewDecoder(r.Body).Decode(&report)
			outcome = report.Outcome
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughReport{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b := followThroughTestBinding(server.URL)
	c, _ := api.NewClient(server.URL, time.Second)
	queued, err := relayFollowThrough(context.Background(), b, &relayProgress{}, c, time.Now().UTC(), func(context.Context, runtimeBinding, string) error {
		queueCalls++
		return nil
	})
	if err != nil || queued || queueCalls != 0 || outcome != api.DeliveryFollowThroughOutcomeInvalidated {
		t.Fatalf("stale revalidation must fence external call: queued=%v queue=%d outcome=%s err=%v", queued, queueCalls, outcome, err)
	}
}

func TestRelayFollowThroughDefinitiveStaleReportDoesNotStarveInbox(t *testing.T) {
	var reportCalls, queueCalls int
	var invalidated api.DeliveryFollowThroughReportRequest
	b := followThroughTestBinding("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/capabilities":
			json.NewEncoder(w).Encode(api.CurrentCapabilities())
		case strings.HasSuffix(r.URL.Path, "/follow-through/report"):
			reportCalls++
			var report api.DeliveryFollowThroughReportRequest
			json.NewDecoder(r.Body).Decode(&report)
			if reportCalls == 1 {
				http.Error(w, `{"error":"stale lease"}`, http.StatusConflict)
				return
			}
			invalidated = report
			json.NewEncoder(w).Encode(api.DeliveryFollowThroughReport{})
		case strings.HasSuffix(r.URL.Path, "/delivery-coverage"):
			json.NewEncoder(w).Encode(api.DeliveryCoverage{Status: api.DeliveryFollowThroughUncovered})
		case strings.Contains(r.URL.Path, "/messages"):
			json.NewEncoder(w).Encode(api.MessageList{Messages: []api.Message{{Seq: 9, To: b.Agent, From: api.Sender{AgentID: "agt_0000000000000002"}, Text: "ordinary current message"}}})
		case strings.Contains(r.URL.Path, "/agents/"):
			json.NewEncoder(w).Encode(api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentDone, Online: true, Unread: 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b.Hub = server.URL
	c, _ := api.NewClient(server.URL, time.Second)
	p := relayProgress{Run: b.Run, Thread: b.Thread, FollowThroughSupported: true, FollowThroughCheckedAt: time.Now().UTC(), PendingFollowThroughDelivery: "dly_0000000000000001", PendingFollowThroughReport: &api.DeliveryFollowThroughReportRequest{
		RequestID: "old-report", AgentID: b.Agent, RunID: b.Run, ExpectedGeneration: 1, ExpectedEpoch: 1,
		LeaseRequestID: "old-lease", Outcome: api.DeliveryFollowThroughOutcomeAccepted,
	}}
	now := p.FollowThroughCheckedAt
	queued, err := relayFollowThrough(context.Background(), b, &p, c, now, func(context.Context, runtimeBinding, string) error {
		queueCalls++
		return nil
	})
	if err != nil || queued || p.PendingFollowThroughReport != nil || reportCalls != 2 || invalidated.Outcome != api.DeliveryFollowThroughOutcomeInvalidated || invalidated.LeaseRequestID != "old-lease" {
		t.Fatalf("definitive stale reconciliation: queued=%v pending=%+v calls=%d invalidated=%+v err=%v", queued, p.PendingFollowThroughReport, reportCalls, invalidated, err)
	}
	if err = relayOne(context.Background(), b, &p, c, now, func(_ context.Context, _ runtimeBinding, prompt string) error {
		queueCalls++
		if !strings.Contains(prompt, "through message #9") {
			t.Fatalf("ordinary inbox prompt lost: %q", prompt)
		}
		return nil
	}); err != nil || queueCalls != 1 {
		t.Fatalf("stale follow-through report starved ordinary inbox: calls=%d err=%v", queueCalls, err)
	}
}

func TestRelayFollowThroughLifecycleAndExecutionRaceNeverQueues(t *testing.T) {
	for _, tc := range []struct {
		name, coverageStatus, agentStatus, phase string
	}{
		{"retired", api.DeliveryFollowThroughRetired, api.AgentRetired, api.DeliveryUnacknowledged},
		{"closed", api.DeliveryFollowThroughClosed, api.AgentClosed, api.DeliveryUnacknowledged},
		{"exited", api.DeliveryFollowThroughClosed, api.AgentExited, api.DeliveryUnacknowledged},
		{"progress", "covered", api.AgentRunning, api.DeliveryProgressing},
		{"blocked", "covered", api.AgentNeedsInput, api.DeliveryBlocked},
		{"result", "covered", api.AgentDone, api.DeliveryResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var coverageCalls, queueCalls int
			var leaseID, outcome string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				b := followThroughTestBinding("")
				d := followThroughTestDelivery(b)
				d.Phase = api.DeliveryUnacknowledged
				switch {
				case r.URL.Path == "/v1/capabilities":
					json.NewEncoder(w).Encode(api.CurrentCapabilities())
				case strings.HasSuffix(r.URL.Path, "/delivery-coverage"):
					coverageCalls++
					if coverageCalls == 1 {
						json.NewEncoder(w).Encode(api.DeliveryCoverage{Status: "covered", AgentStatus: api.AgentRunning, Delivery: &d})
						return
					}
					d.Phase = tc.phase
					if tc.coverageStatus != "covered" {
						d.FollowThrough.PendingAction = api.DeliveryFollowThroughActionQueue
						d.FollowThrough.PendingRequestID = leaseID
					}
					json.NewEncoder(w).Encode(api.DeliveryCoverage{Status: tc.coverageStatus, AgentStatus: tc.agentStatus, Delivery: &d})
				case strings.HasSuffix(r.URL.Path, "/follow-through/check"):
					var check api.DeliveryFollowThroughCheckRequest
					json.NewDecoder(r.Body).Decode(&check)
					leaseID = check.RequestID
					json.NewEncoder(w).Encode(api.DeliveryFollowThroughDecision{Delivery: d, Action: api.DeliveryFollowThroughActionQueue, Execute: true})
				case strings.HasSuffix(r.URL.Path, "/follow-through/report"):
					var report api.DeliveryFollowThroughReportRequest
					json.NewDecoder(r.Body).Decode(&report)
					outcome = report.Outcome
					json.NewEncoder(w).Encode(api.DeliveryFollowThroughReport{})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			b := followThroughTestBinding(server.URL)
			c, _ := api.NewClient(server.URL, time.Second)
			queued, err := relayFollowThrough(context.Background(), b, &relayProgress{}, c, time.Now().UTC(), func(context.Context, runtimeBinding, string) error {
				queueCalls++
				return nil
			})
			if err != nil || queued || queueCalls != 0 || coverageCalls != 2 || outcome != api.DeliveryFollowThroughOutcomeInvalidated {
				t.Fatalf("race must cancel before external queue: queued=%v queue=%d coverage=%d outcome=%s err=%v", queued, queueCalls, coverageCalls, outcome, err)
			}
		})
	}
}
