package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 3 (docs/broker-phase-3.md).

// c1: legacy writers answer 410 Gone; c10: the bridge reaches the new owner
// routes and still nothing else.
func TestPhase3Routes(t *testing.T) {
	h := newTokenHub(t)
	task, builder, o := h.project()
	base := "/v1/tasks/" + task.ID
	for _, path := range []string{
		base + "/required-deliveries",
		base + "/deliveries/dly_0000000000000000/follow-through/check",
		base + "/deliveries/dly_0000000000000000/follow-through/report",
		base + "/operational-records",
		base + "/operational-records/opr_0000000000000000/commit",
		base + "/deliveries/dly_0000000000000000/ack",
		base + "/deliveries/dly_0000000000000000/recovery-incidents",
		base + "/deliveries/dly_0000000000000000/blocks/blk_0000000000000000/resolutions",
	} {
		var e api.ErrorResponse
		if code, _ := h.do(ownerToken, "POST", path, map[string]any{}, &e); code != http.StatusGone || e.Code != "retired" {
			t.Errorf("POST %s = %d %+v, want 410 retired", path, code, e)
		}
	}
	var caps api.Capabilities
	h.do(ownerToken, "GET", "/v1/capabilities", nil, &caps)
	// Reads stay advertised (old and new tt keep reading); writes are retired.
	if !caps.ReliableDelivery.Supported || !caps.ReliableDelivery.WritesRetired || !caps.OperationalRecords.Supported || !caps.OperationalRecords.WritesRetired {
		t.Errorf("capabilities = %+v %+v, want reads supported and writes retired", caps.ReliableDelivery, caps.OperationalRecords)
	}
	var out api.OwnerActionResult
	if code, _ := h.do(bridgeToken, "POST", base+"/obligations/"+o.ID+"/extend", api.ObligationExtendRequest{For: "30m", RequestID: "discord-interaction-1"}, &out); code != http.StatusCreated || out.Obligation == nil {
		t.Fatalf("bridge extend = %d %+v", code, out)
	}
	if code, _ := h.do(bridgeToken, "POST", base+"/obligations/"+o.ID+"/extend", api.ObligationExtendRequest{For: "30m", RequestID: "discord-interaction-1"}, &out); code != http.StatusOK || !out.Replay {
		t.Fatalf("a retried extend = %d %+v", code, out)
	}
	if code, _ := h.do(bridgeToken, "POST", base+"/obligations/"+o.ID+"/cancel", api.ObligationCancelRequest{Reason: "not needed", RequestID: "discord-interaction-2"}, &out); code != http.StatusCreated {
		t.Fatalf("bridge cancel = %d", code)
	}
	if code, _ := h.do(bridgeToken, "POST", base+"/obligations/"+o.ID+"/answer", api.ObligationAnswerRequest{Text: "x", RequestID: "discord-interaction-3"}, nil); code != http.StatusConflict {
		t.Fatalf("answering a closed obligation = %d, want 409", code)
	}
	if code, _ := h.do(bridgeToken, "POST", base+"/agents/"+builder.ID+"/resume", api.AgentResumeRequest{RequestID: "discord-interaction-4"}, nil); code != http.StatusConflict {
		t.Fatalf("resuming an active agent = %d, want 409", code)
	}
	if code, _ := h.do(bridgeToken, "PATCH", base+"/agents/"+builder.ID, map[string]any{"status": "retired"}, nil); code != http.StatusForbidden {
		t.Fatalf("the bridge edited an agent directly: %d", code)
	}
}

// Phase 3.1 k1: the refusal reaches clients as 409 unacknowledged with the fix.
func TestAckGateOverHTTP(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	lead := h.agentNamed(task, "lead")
	later := time.Now().UTC().Add(time.Hour)
	h.st.SetClockForTest(func() time.Time { return later })
	var e api.ErrorResponse
	code, _ := h.do(ownerToken, "POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{AgentID: builder.ID, RunID: builder.RunID, To: lead.ID, Text: "status: still working"}, &e)
	if code != http.StatusConflict || e.Code != "unacknowledged" || !strings.Contains(e.Error, "tt ack") {
		t.Fatalf("gated post = %d %+v", code, e)
	}
}
