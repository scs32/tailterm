package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestHandlerResponseSummaryUsesCreationAndOutcome(t *testing.T) {
	start := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := start.Add(d); return &v }
	list := []api.Obligation{
		{MessageSeq: 10, HandlerRequest: true, State: api.ObligationClosed, Outcome: api.OutcomeResult, OutcomeSeq: 20, CreatedAt: start, ClosedAt: at(2 * time.Minute)},
		{MessageSeq: 11, HandlerRequest: true, State: api.ObligationClosed, Outcome: api.OutcomeDeclined, OutcomeSeq: 21, CreatedAt: start, ClosedAt: at(4 * time.Minute)},
		{MessageSeq: 12, HandlerRequest: true, State: api.ObligationClosed, Outcome: api.OutcomeResult, OutcomeSeq: 22, CreatedAt: start, ClosedAt: at(6 * time.Minute)},
		{MessageSeq: 13, HandlerRequest: true, State: api.ObligationBlocked, CreatedAt: start},
		{MessageSeq: 14, HandlerRequest: true, State: api.ObligationClosed, Outcome: api.OutcomeCancelled, ClosedAt: at(time.Minute), CreatedAt: start},
		{MessageSeq: 15, HandlerRequest: true, State: api.ObligationClosed, Outcome: api.OutcomeWithdrawn, ClosedAt: at(time.Minute), CreatedAt: start},
		{MessageSeq: 16, HandlerRequest: false, State: api.ObligationClosed, Outcome: api.OutcomeResult, ClosedAt: at(time.Hour), CreatedAt: start},
	}
	now := start.Add(8 * time.Minute)
	got := summarizeHandlerResponses(list, time.Time{}, now)
	for _, want := range []string{"1 pending (1 blocked), oldest 8m0s (#13)", "3 result/decline (1 declined), median 4m0s, max 6m0s (#12→#22)", "cancelled: 1, withdrawn: 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q missing %q", got, want)
		}
	}
	// Delivery, acknowledgement, a done turn and retry bookkeeping are not
	// response events. The summary depends on request/outcome timestamps only.
	list[0].DeliveredAt = at(time.Minute)
	list[0].AckedAt = at(time.Minute)
	list[0].LastProgressAt = at(90 * time.Second)
	if again := summarizeHandlerResponses(list, time.Time{}, now); again != got {
		t.Fatalf("ack changed response summary: %q", again)
	}
}

func TestHandlerResponseCLIFixtureRendersOwnerSummaryAndExactPair(t *testing.T) {
	start := time.Now().UTC().Add(-8 * time.Minute)
	closed := start.Add(6 * time.Minute)
	rows := []api.Obligation{
		{MessageSeq: 12, AgentID: "agt_0000000000000001", HandlerRequest: true, State: api.ObligationClosed, Outcome: api.OutcomeResult, OutcomeSeq: 22, CreatedAt: start, ClosedAt: &closed, ResponseMillis: 360000},
		{MessageSeq: 13, AgentID: "agt_0000000000000001", HandlerRequest: true, HandlerPriority: true, State: api.ObligationBlocked, CreatedAt: start, PendingAgeMillis: 480000},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/message-checks"):
			_ = json.NewEncoder(w).Encode(api.MessageCheckList{Checks: []api.MessageCheck{}})
		case strings.HasSuffix(r.URL.Path, "/agents"):
			_ = json.NewEncoder(w).Encode(api.AgentList{Agents: []api.Agent{}})
		case strings.HasSuffix(r.URL.Path, "/obligations"):
			_ = json.NewEncoder(w).Encode(api.ObligationList{Obligations: rows})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001", runID: "run_0000000000000001"}
	summary, err := captureCLIOutput(t, func() error { return cmdMessageChecks(e, []string{"--summary", "--since", "0"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 pending (1 blocked)", "1 result/decline (0 declined), median 6m0s, max 6m0s (#12→#22)"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("owner summary %q missing %q", summary, want)
		}
	}
	all, err := captureCLIOutput(t, func() error { return cmdObligations(e, []string{"--all"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "handler response #12→#22 6m0s") || !strings.Contains(all, "handler request pending 8m0s, live gate, blocked") {
		t.Fatalf("per-request CLI output: %q", all)
	}
}
