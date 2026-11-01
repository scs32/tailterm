package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The hub derives evidence exits and the failed-command flag while it
// normalizes a post (api.DeriveEvidence). A post's retry identity hashes the
// request without them (messageHashForm), so these tests pin that a retry
// keeps replaying across the upgrade and across tt versions.

func upgradeRetryResult(outcome string) *api.Envelope {
	return &api.Envelope{Kind: api.EnvelopeKindResult, Subject: "Tests ran for the upgrade retry fixture",
		Body:     api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
		Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "go test ./x", Outcome: outcome}}}
}

func storedPostHash(t *testing.T, f deliveryFixture, requestID string) string {
	t.Helper()
	var hash string
	if err := f.s.db.QueryRow(`SELECT payload_hash FROM message_post_requests WHERE task_id=? AND request_id=?`, f.task.ID, requestID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func upgradeRetryMessageCount(t *testing.T, f deliveryFixture) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMessageRetryUpgradeReplaysPreUpgradeRequest(t *testing.T) {
	// Golden: requestHash of this exact normalized request at tasks-hub
	// f03f6a56, the hub before evidence exits and flags were derived.
	const golden = "16ca0807adbce599425fa351375aa0ba5ce3e46bf2b1ec759bbec057ab9316a7"
	req := api.PostMessageRequest{RequestID: "upgrade-retry", Envelope: upgradeRetryResult("exit 1; 2 failed")}
	normalized := req
	env := *req.Envelope
	normalized.Envelope = &env
	if err := api.NormalizeEnvelopePost(&normalized); err != nil {
		t.Fatal(err)
	}
	if len(normalized.Envelope.Flags) != 1 || normalized.Envelope.Evidence["e1"].Exit == nil {
		t.Fatalf("fixture should be flagged: %+v", normalized.Envelope)
	}
	if got := requestHash(messageHashForm(normalized)); got != golden {
		t.Fatalf("hash form %s, want the pre-upgrade %s", got, golden)
	}
	if requestHash(normalized) == golden {
		t.Fatal("the full request hashes the same; the fixture does not exercise derived fields")
	}

	f := newDeliveryFixture(t)
	ctx := context.Background()
	first, err := f.s.PostMessage(ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if first.Envelope == nil || len(first.Envelope.Flags) != 1 {
		t.Fatalf("stored envelope %+v", first.Envelope)
	}
	before := upgradeRetryMessageCount(t, f)
	// Simulate a receipt the pre-upgrade hub wrote for the same request.
	if _, err := f.s.db.Exec(`UPDATE message_post_requests SET payload_hash=? WHERE task_id=? AND request_id=?`, golden, f.task.ID, "upgrade-retry"); err != nil {
		t.Fatal(err)
	}
	again, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: "upgrade-retry", Envelope: upgradeRetryResult("exit 1; 2 failed")}, f.by)
	if err != nil || again.Seq != first.Seq {
		t.Fatalf("retry across the upgrade: seq %d vs %d, %v", again.Seq, first.Seq, err)
	}
	if upgradeRetryMessageCount(t, f) != before {
		t.Fatal("the retry stored another message")
	}
}

func TestMessageRetryUpgradeOldAndNewClientShapesReplay(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	// An old tt sends no exit field.
	first, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: "shapes", Envelope: upgradeRetryResult("exit 1; 2 failed")}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	// A new tt sends the exit it derived; a forged flag is replaced, not hashed.
	newer := upgradeRetryResult("exit 1; 2 failed")
	one := 1
	newer.Evidence["e1"] = api.Evidence{Type: "command", Value: "go test ./x", Outcome: "exit 1; 2 failed", Exit: &one}
	newer.Flags = []api.EnvelopeFlag{{Kind: "failed_command", Field: "evidence.e9", Reason: "forged"}}
	again, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: "shapes", Envelope: newer}, f.by)
	if err != nil || again.Seq != first.Seq {
		t.Fatalf("new client shape: seq %d vs %d, %v", again.Seq, first.Seq, err)
	}
	if len(again.Envelope.Flags) != 1 || again.Envelope.Flags[0].Field != "evidence.e1" {
		t.Fatalf("replayed flags %+v", again.Envelope.Flags)
	}
}

func TestMessageRetryUpgradeStillRefusesChangedData(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	if _, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: "changed", Envelope: upgradeRetryResult("exit 1; 2 failed")}, f.by); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: "changed", Envelope: upgradeRetryResult("exit 0; ok")}, f.by)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "request ID was already used with different message data") {
		t.Fatalf("changed data: %v", err)
	}
}

// Lead decision L2: the hub's own request ids for an owner request and an
// owner reply hash the same derived-free form, so an exact replay with the
// returned id replays, and the stored hash leaves the derived fields out.
func TestMessageRetryUpgradeAutoKeyedOwnerPostsReplay(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	evidence := func() map[string]api.Evidence {
		return map[string]api.Evidence{"e1": {Type: "command", Value: "go test ./x", Outcome: "exit 1; 2 failed"}}
	}
	request := func() api.PostMessageRequest {
		// The board-wide form: the hub hashes it as sent (a To of "owner" is
		// cleared before hashing, so its replay would need To "").
		return api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest,
			Subject: "Approve the failed check for the fixture", Body: api.EnvelopeBody{Ask: "Please decide"}, Evidence: evidence()}}
	}
	owner, err := f.s.PostMessage(ctx, f.task.ID, request(), f.by)
	if err != nil {
		t.Fatal(err)
	}
	if owner.PostReceipt == nil || !strings.HasPrefix(owner.PostReceipt.RequestID, "ownerrequest") || owner.Envelope.Evidence["e1"].Exit == nil {
		t.Fatalf("owner request %+v receipt %+v", owner.Envelope, owner.PostReceipt)
	}
	replay := request()
	replay.RequestID, replay.WorkItems, replay.WorkOrderMessage = owner.PostReceipt.RequestID, owner.WorkItems, owner.WorkOrderMessage
	checkStoredHash(t, f, replay)
	again, err := f.s.PostMessage(ctx, f.task.ID, replay, f.by)
	if err != nil || again.Seq != owner.Seq {
		t.Fatalf("owner request replay: seq %d vs %d, %v", again.Seq, owner.Seq, err)
	}

	answer := func() api.PostMessageRequest {
		return api.PostMessageRequest{ReplyTo: owner.Seq, Envelope: &api.Envelope{Kind: api.EnvelopeKindAnswer, Subject: "The owner accepts the failed check",
			Body: api.EnvelopeBody{Answer: "Accepted"}, Evidence: evidence()}}
	}
	reply, err := f.s.PostMessage(ctx, f.task.ID, answer(), f.by)
	if err != nil {
		t.Fatal(err)
	}
	if reply.PostReceipt == nil || !strings.HasPrefix(reply.PostReceipt.RequestID, "ownerreply") {
		t.Fatalf("owner reply receipt %+v", reply.PostReceipt)
	}
	replayReply := answer()
	replayReply.RequestID, replayReply.WorkItems, replayReply.WorkOrderMessage = reply.PostReceipt.RequestID, reply.WorkItems, reply.WorkOrderMessage
	checkStoredHash(t, f, replayReply)
	again, err = f.s.PostMessage(ctx, f.task.ID, replayReply, f.by)
	if err != nil || again.Seq != reply.Seq {
		t.Fatalf("owner reply replay: seq %d vs %d, %v", again.Seq, reply.Seq, err)
	}
}

// checkStoredHash: the receipt holds the derived-free hash of the request as
// the hub normalized it, which differs from the full request's hash.
func checkStoredHash(t *testing.T, f deliveryFixture, req api.PostMessageRequest) {
	t.Helper()
	env := *req.Envelope
	req.Envelope = &env
	if err := api.NormalizeEnvelopePost(&req); err != nil {
		t.Fatal(err)
	}
	stored := storedPostHash(t, f, req.RequestID)
	if stored != requestHash(messageHashForm(req)) || stored == requestHash(req) {
		t.Fatalf("%s: stored hash %s is not the derived-free form", req.RequestID, stored)
	}
}
