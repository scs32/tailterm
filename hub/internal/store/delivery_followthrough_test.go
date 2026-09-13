package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func enrolledDirective(t *testing.T, f deliveryFixture, key string) api.DeliveryMutation {
	t.Helper()
	governing := contextLinkedMessage(t, f.s, f.task, f.item, "Governing execution follow-through order", key+"-order", nil)
	governingRef := api.MessageReference{TaskID: f.task.ID, Seq: governing.Seq}
	text := "Full immutable instruction bytes for " + key
	message := contextLinkedMessage(t, f.s, f.task, f.item, text, key+"-message", &governingRef)
	bytes, digest := instructionEvidence(text)
	out, err := f.s.CreateRequiredDelivery(context.Background(), f.task.ID, api.CreateRequiredDeliveryRequest{
		RequestID: key, MessageSeq: message.Seq, Kind: api.DeliveryAssignment,
		AgentID: f.worker.ID, RunID: f.worker.RunID, ItemTaskID: f.item.TaskID, ItemID: f.item.ID,
		ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq},
		GoverningOrderMessage: governingRef, InstructionSHA256: digest, InstructionBytes: bytes,
		EnrollmentVersion: api.ReliableFollowThroughCapabilityVersion,
		ProducerAgentID:   f.lead.ID, ProducerRunID: f.lead.RunID,
	}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func followThroughCheck(f deliveryFixture, d api.RequiredDelivery, key string, observed time.Time, state string) api.DeliveryFollowThroughCheckRequest {
	return api.DeliveryFollowThroughCheckRequest{
		RequestID: key, AgentID: f.worker.ID, RunID: f.worker.RunID,
		ExpectedGeneration: d.Generation, ExpectedEpoch: d.ExecutionEpoch,
		Observation: api.DeliveryRuntimeObservation{State: state, Source: "synthetic-supported-observer", ObservedAt: observed},
	}
}

func TestDeliveryCoverageRequiresExactFullInstructionEnrollment(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	coverage, err := f.s.DeliveryCoverage(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || coverage.Status != api.DeliveryFollowThroughUncovered || coverage.ItemID != f.item.ID || coverage.ContextDigest == "" {
		t.Fatalf("bound but unenrolled worker must remain visibly uncovered: %+v %v", coverage, err)
	}

	governing := contextLinkedMessage(t, f.s, f.task, f.item, "Actual governing order", "evidence-order", nil)
	governingRef := api.MessageReference{TaskID: f.task.ID, Seq: governing.Seq}
	text := "Complete recovered instruction text"
	message := contextLinkedMessage(t, f.s, f.task, f.item, text, "evidence-message", &governingRef)
	bytes, digest := instructionEvidence(text)
	base := api.CreateRequiredDeliveryRequest{
		RequestID: "bad-evidence", MessageSeq: message.Seq, Kind: api.DeliveryAssignment,
		AgentID: f.worker.ID, RunID: f.worker.RunID, ItemTaskID: f.item.TaskID, ItemID: f.item.ID,
		ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq},
		GoverningOrderMessage: governingRef, InstructionSHA256: digest, InstructionBytes: bytes,
		EnrollmentVersion: api.ReliableFollowThroughCapabilityVersion,
		ProducerAgentID:   f.lead.ID, ProducerRunID: f.lead.RunID,
	}
	bad := base
	bad.GoverningOrderMessage = base.WorkOrderMessage // A nearby checkpoint/order cannot replace the authored governing reference.
	if _, err = f.s.CreateRequiredDelivery(ctx, f.task.ID, bad, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong governing order should conflict: %v", err)
	}
	bad = base
	bad.RequestID = "bad-instruction-digest"
	bad.InstructionSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err = f.s.CreateRequiredDelivery(ctx, f.task.ID, bad, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("incomplete or changed instruction should conflict: %v", err)
	}
	base.RequestID = "good-evidence"
	created, err := f.s.CreateRequiredDelivery(ctx, f.task.ID, base, f.by)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err = f.s.DeliveryCoverage(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || coverage.Status != "covered" || coverage.Delivery == nil || coverage.Delivery.ID != created.Delivery.ID || coverage.Delivery.InstructionSHA256 != digest || coverage.Delivery.GoverningOrderMessage != governingRef {
		t.Fatalf("verified enrollment coverage: %+v %v", coverage, err)
	}
}

func TestFollowThroughHeartbeatAndReadDoNotPreventOverdueLease(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	f.s.now = func() time.Time { return now }
	d := enrolledDirective(t, f, "overdue")
	before := followThroughCheck(f, d.Delivery, "check-before", now, api.DeliveryObservationUnknown)
	decision, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, before, f.by)
	if err != nil || decision.Execute || decision.Classification != api.DeliveryFollowThroughAwaitingAck {
		t.Fatalf("before deadline: %+v %v", decision, err)
	}
	if err = f.s.MarkRead(ctx, f.task.ID, api.MarkReadRequest{AgentID: f.worker.ID, UpTo: d.Delivery.MessageSeq}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(121 * time.Second)
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: "heartbeat", AgentID: f.worker.ID, RunID: f.worker.RunID}, f.by); err != nil {
		t.Fatal(err)
	}
	check := followThroughCheck(f, d.Delivery, "check-overdue", now, api.DeliveryObservationUnknown)
	decision, err = f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, check, f.by)
	if err != nil || !decision.Execute || decision.Action != api.DeliveryFollowThroughActionQueue || decision.Attempt != 1 {
		t.Fatalf("heartbeat/read cursor cannot satisfy substantive deadline: %+v %v", decision, err)
	}
	replay, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, check, f.by)
	if err != nil || !replay.Replay || replay.Execute || replay.Receipt == nil || decision.Receipt == nil || replay.Receipt.ID != decision.Receipt.ID {
		t.Fatalf("lease replay must not repeat external side effect: %+v %v", replay, err)
	}
}

func TestFollowThroughUnreportedLeaseExpiresOnceAcrossRestart(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 18, 30, 0, 0, time.UTC)
	f.s.now = func() time.Time { return now }
	d := enrolledDirective(t, f, "crash")
	now = now.Add(121 * time.Second)
	lease, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, d.Delivery, "crash-lease", now, api.DeliveryObservationUnknown), f.by)
	if err != nil || !lease.Execute {
		t.Fatalf("lease: %+v %v", lease, err)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.s = reopened
	t.Cleanup(func() { reopened.Close() })
	now = now.Add(31 * time.Second)
	reopened.now = func() time.Time { return now }
	escalated, err := reopened.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, d.Delivery, "crash-expired", now, api.DeliveryObservationUnknown), f.by)
	if err != nil || escalated.Execute || escalated.Classification != api.DeliveryFollowThroughEscalated || escalated.EscalationMessageSeq < 1 {
		t.Fatalf("unreported lease must become one durable ambiguous escalation: %+v %v", escalated, err)
	}
	again, err := reopened.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, d.Delivery, "crash-again", now.Add(time.Second), api.DeliveryObservationUnknown), f.by)
	if err != nil || again.Execute || again.Classification != api.DeliveryFollowThroughEscalated || again.Event != nil {
		t.Fatalf("escalation must not repeat queue or message side effects: %+v %v", again, err)
	}
	var leases, escalations int
	if err = reopened.db.QueryRow(`SELECT count(*) FROM delivery_events WHERE delivery_id=? AND kind='followthrough_queue_leased'`, d.Delivery.ID).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err = reopened.db.QueryRow(`SELECT count(*) FROM delivery_events WHERE delivery_id=? AND kind='followthrough_escalated'`, d.Delivery.ID).Scan(&escalations); err != nil {
		t.Fatal(err)
	}
	if leases != 1 || escalations != 1 {
		t.Fatalf("unexpected side-effect counts: leases=%d escalations=%d", leases, escalations)
	}
}

func TestFollowThroughActiveToolFreshnessAndSubstantiveAck(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 19, 0, 0, 0, time.UTC)
	f.s.now = func() time.Time { return now }
	d := enrolledDirective(t, f, "active-tool")
	now = now.Add(121 * time.Second)
	active := followThroughCheck(f, d.Delivery, "fresh-tool", now, api.DeliveryObservationActiveTool)
	decision, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, active, f.by)
	if err != nil || decision.Execute || decision.Classification != api.DeliveryFollowThroughActiveTool {
		t.Fatalf("fresh active tool should defer without interrupting: %+v %v", decision, err)
	}
	stale := followThroughCheck(f, d.Delivery, "stale-tool", now.Add(3*time.Minute), api.DeliveryObservationActiveTool)
	stale.Observation.ObservedAt = now
	f.s.now = func() time.Time { return now.Add(3 * time.Minute) }
	if _, err = f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, stale, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("stale active-tool observation must not suppress indefinitely: %v", err)
	}
	ack, err := f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "actual-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by)
	if err != nil || ack.Delivery.Phase != api.DeliveryAcknowledged || ack.Delivery.FollowThrough.AttemptCount != 0 {
		t.Fatalf("actual exact directive ack should resolve the incident state: %+v %v", ack, err)
	}
}

func TestFollowThroughAcceptedQueueStillNeedsConsumptionAndBlocksStayDistinct(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC)
	f.s.now = func() time.Time { return now }
	d := enrolledDirective(t, f, "accepted-unconfirmed")
	now = now.Add(121 * time.Second)
	first, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, d.Delivery, "accepted-check-1", now, api.DeliveryObservationUnknown), f.by)
	if err != nil || !first.Execute {
		t.Fatalf("first queue lease: %+v %v", first, err)
	}
	report := api.DeliveryFollowThroughReportRequest{RequestID: "accepted-report-1", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedGeneration: 1, ExpectedEpoch: 1, LeaseRequestID: "accepted-check-1", Outcome: api.DeliveryFollowThroughOutcomeAccepted, Text: "queue receipt only"}
	if _, err = f.s.ReportDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, report, f.by); err != nil {
		t.Fatal(err)
	}
	now = now.Add(121 * time.Second)
	second, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, d.Delivery, "accepted-check-2", now, api.DeliveryObservationUnknown), f.by)
	if err != nil || !second.Execute || second.Attempt != 2 {
		t.Fatalf("queue acceptance without consumption must remain unconfirmed: %+v %v", second, err)
	}
	report.RequestID, report.LeaseRequestID = "accepted-report-2", "accepted-check-2"
	if _, err = f.s.ReportDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, report, f.by); err != nil {
		t.Fatal(err)
	}
	now = now.Add(121 * time.Second)
	escalated, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, d.Delivery, "accepted-escalate", now, api.DeliveryObservationUnknown), f.by)
	if err != nil || escalated.Classification != api.DeliveryFollowThroughEscalated || escalated.Execute {
		t.Fatalf("bounded accepted attempts without consumption must escalate: %+v %v", escalated, err)
	}

	// A real worker ack is substantive evidence and starts a fresh progress
	// deadline. An explicit durable dependency block then suppresses all wake
	// attempts without being mislabeled idle or recovered.
	if _, err = f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "accepted-actual-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by); err != nil {
		t.Fatal(err)
	}
	blocked, err := f.s.BlockDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryBlockRequest{RequestID: "accepted-block", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, ReasonClass: "dependency", Text: "known synthetic dependency"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	decision, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, blocked.Delivery, "blocked-check", now, api.DeliveryObservationUnknown), f.by)
	if err != nil || decision.Classification != api.DeliveryFollowThroughKnownBlock || decision.Execute {
		t.Fatalf("known block must remain distinct: %+v %v", decision, err)
	}
}

func TestFollowThroughCompetingTicksLeaseOneExternalAction(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 22, 0, 0, 0, time.UTC)
	f.s.now = func() time.Time { return now }
	d := enrolledDirective(t, f, "competing")
	now = now.Add(121 * time.Second)
	requests := []api.DeliveryFollowThroughCheckRequest{
		followThroughCheck(f, d.Delivery, "competing-a", now, api.DeliveryObservationUnknown),
		followThroughCheck(f, d.Delivery, "competing-b", now, api.DeliveryObservationUnknown),
	}
	decisions := make([]api.DeliveryFollowThroughDecision, len(requests))
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			decisions[index], errs[index] = f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, requests[index], f.by)
		}(i)
	}
	wg.Wait()
	executes := 0
	for i := range decisions {
		if errs[i] != nil {
			t.Fatalf("competing check %d: %v", i, errs[i])
		}
		if decisions[i].Execute {
			executes++
		}
	}
	var leaseEvents int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM delivery_events WHERE delivery_id=? AND kind='followthrough_queue_leased'`, d.Delivery.ID).Scan(&leaseEvents); err != nil {
		t.Fatal(err)
	}
	if executes != 1 || leaseEvents != 1 {
		t.Fatalf("competing ticks must own exactly one external action: executes=%d events=%d decisions=%+v", executes, leaseEvents, decisions)
	}
}

func TestFollowThroughLeaseInvalidationPreservesProgressAndLifecycle(t *testing.T) {
	for _, transition := range []string{"progress", api.AgentRetired, api.AgentClosed, api.AgentExited} {
		t.Run(transition, func(t *testing.T) {
			f := newDeliveryFixture(t)
			ctx := context.Background()
			now := time.Date(2026, 9, 13, 23, 0, 0, 0, time.UTC)
			f.s.now = func() time.Time { return now }
			d := enrolledDirective(t, f, "invalidate-"+transition)
			now = now.Add(121 * time.Second)
			leaseKey := "invalidate-lease-" + transition
			lease, err := f.s.CheckDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, followThroughCheck(f, d.Delivery, leaseKey, now, api.DeliveryObservationUnknown), f.by)
			if err != nil || !lease.Execute {
				t.Fatalf("lease: %+v %v", lease, err)
			}
			if transition == "progress" {
				if _, err = f.s.AcknowledgeDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "invalidate-ack", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1}, f.by); err != nil {
					t.Fatal(err)
				}
				if _, err = f.s.ProgressDelivery(ctx, f.task.ID, d.Delivery.ID, api.DeliveryActionRequest{RequestID: "invalidate-progress", AgentID: f.worker.ID, RunID: f.worker.RunID, ExpectedEpoch: 1, Text: "genuine progress won the race"}, f.by); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = f.s.UpdateAgent(ctx, f.worker.ID, api.UpdateAgentRequest{Status: &transition}, f.by); err != nil {
					t.Fatal(err)
				}
				coverage, coverageErr := f.s.DeliveryCoverage(ctx, f.task.ID, f.worker.ID, f.worker.RunID)
				want := api.DeliveryFollowThroughClosed
				if transition == api.AgentRetired {
					want = api.DeliveryFollowThroughRetired
				}
				if coverageErr != nil || coverage.Status != want || coverage.AgentStatus != transition {
					t.Fatalf("lifecycle coverage: %+v %v", coverage, coverageErr)
				}
			}
			report, err := f.s.ReportDeliveryFollowThrough(ctx, f.task.ID, d.Delivery.ID, api.DeliveryFollowThroughReportRequest{
				RequestID: "invalidate-report-" + transition, AgentID: f.worker.ID, RunID: f.worker.RunID,
				ExpectedGeneration: 1, ExpectedEpoch: 1, LeaseRequestID: leaseKey,
				Outcome: api.DeliveryFollowThroughOutcomeInvalidated, Text: "exact lease invalidated before external call",
			}, f.by)
			if err != nil || report.Event.Kind != "followthrough_queue_invalidated" {
				t.Fatalf("invalidated report: %+v %v", report, err)
			}
			if transition == "progress" && (report.Delivery.Phase != api.DeliveryProgressing || report.Delivery.FollowThrough.LastSubstantiveProgressAt == nil) {
				t.Fatalf("lease invalidation erased genuine progress: %+v", report.Delivery)
			}
		})
	}
}
