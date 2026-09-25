package main

import (
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestMessageChecksSummarySeparatesWithdrawnFromUnacknowledged(t *testing.T) {
	now := time.Now().UTC()
	rows := []api.Obligation{{MessageSeq: 10, Needs: api.ObligationNeedsOutcome, State: api.ObligationClosed, Outcome: api.OutcomeWithdrawn, CreatedAt: now.Add(-time.Hour), AckDueAt: now.Add(-50 * time.Minute)}}
	out := summarizeAcks(rows, nil, now.Add(-2*time.Hour), now)
	if !strings.Contains(out, "Withdrawn: 1") || strings.Contains(out, "Unacknowledged past") {
		t.Fatal(out)
	}
	acked := now.Add(-55 * time.Minute)
	rows[0].AckedAt = &acked
	out = summarizeAcks(rows, nil, now.Add(-2*time.Hour), now)
	if !strings.Contains(out, "1 acknowledged") || !strings.Contains(out, "Withdrawn: 1") {
		t.Fatalf("withdrawal erased acknowledgement history: %s", out)
	}
}
