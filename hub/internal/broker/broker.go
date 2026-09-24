// Package broker is the deterministic obligation scheduler for broker phase
// 2a (docs/broker-phase-2a.md). Each Tick reads durable obligation state and
// takes at most one step per obligation: re-wake, nudge, escalate, or close
// when the recipient is gone. A restarted hub makes the same decisions, so a
// deadline missed during downtime fires once rather than once per missed tick.
package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

type Broker struct {
	Store    *store.Store
	Interval time.Duration
	Log      func(format string, args ...any)
}

// Step is one action a Tick took, for logs and tests.
type Step struct {
	TaskID       string
	ObligationID string
	MessageSeq   int64
	Action       string // wake, nudge, escalate-lead, escalate-owner, recipient-gone, project-stalled
}

func wakesDue(o store.BrokerObligation, now time.Time) int {
	due := 1 // the wake queued with the message
	for _, offset := range api.ObligationWakeOffsets {
		if !now.Before(o.CreatedAt.Add(offset)) {
			due++
		}
	}
	return due
}

// Tick runs one scheduling pass at now.
func (b *Broker) Tick(ctx context.Context, now time.Time) ([]Step, error) {
	open, err := b.Store.BrokerOpenObligations(ctx)
	if err != nil {
		return nil, err
	}
	var steps []Step
	record := func(o store.BrokerObligation, action string, err error) error {
		if err != nil {
			return fmt.Errorf("%s obligation %s: %w", action, o.ID, err)
		}
		steps = append(steps, Step{TaskID: o.TaskID, ObligationID: o.ID, MessageSeq: o.MessageSeq, Action: action})
		return nil
	}
	type taskState struct {
		lastChange time.Time
		overdue    []store.BrokerObligation
	}
	tasks := map[string]*taskState{}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, o := range open {
		if o.TaskPaused {
			continue // owner-paused projects are neither woken nor escalated
		}
		if o.AgentStatus == api.AgentClosed || o.AgentStatus == api.AgentExited || o.AgentStatus == "" {
			keep(record(o, "recipient-gone", b.Store.BrokerCloseRecipientGone(ctx, o, now)))
			continue
		}
		t := tasks[o.TaskID]
		if t == nil {
			t = &taskState{}
			tasks[o.TaskID] = t
		}
		if o.ChangedAt.After(t.lastChange) {
			t.lastChange = o.ChangedAt
		}
		if o.State == api.ObligationQueued || o.State == api.ObligationDelivered {
			if o.Wakes < wakesDue(o, now) {
				keep(record(o, "wake", b.Store.BrokerWake(ctx, o, now)))
			}
		}
		overdue := store.ObligationOverdue(o.Obligation, now)
		if overdue == "" {
			continue
		}
		// A project counts as stalled only after its overdue work has already
		// gone to the lead, so the owner never hears before the lead does.
		if o.Escalation >= 1 {
			t.overdue = append(t.overdue, o)
			if o.EscalatedAt.After(t.lastChange) {
				t.lastChange = o.EscalatedAt
			}
		}
		switch {
		case overdue == "silence" && o.Nudges < api.ObligationMaxNudges:
			if o.NudgedAt.IsZero() || now.Sub(o.NudgedAt) >= api.ObligationSilenceNudge {
				keep(record(o, "nudge", b.Store.BrokerNudge(ctx, o, now)))
			}
		case o.Escalation == 0:
			keep(record(o, "escalate-lead", b.Store.BrokerEscalate(ctx, o, 1, overdueReason(overdue), now)))
		case o.Escalation == 1 && now.Sub(o.EscalatedAt) >= api.ObligationOwnerAfterLead:
			keep(record(o, "escalate-owner", b.Store.BrokerEscalate(ctx, o, 2, overdueReason(overdue)+"; the lead escalation went unanswered", now)))
		}
	}
	for taskID, t := range tasks {
		notified, err := b.Store.BrokerProjectStall(ctx, taskID, t.lastChange, t.overdue, now)
		keep(err)
		if notified {
			steps = append(steps, Step{TaskID: taskID, Action: "project-stalled"})
		}
	}
	return steps, firstErr
}

func overdueReason(kind string) string {
	switch kind {
	case "ack":
		return fmt.Sprintf("not acknowledged within %s", api.ObligationAckDeadline)
	case "silence":
		return fmt.Sprintf("no progress after %d reminders", api.ObligationMaxNudges)
	}
	return "past its due time"
}

// Start runs Tick every Interval until ctx ends; the returned channel closes on exit.
func (b *Broker) Start(ctx context.Context) <-chan struct{} {
	if b.Interval <= 0 {
		b.Interval = 30 * time.Second
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(b.Interval)
		defer ticker.Stop()
		for {
			steps, err := b.Tick(ctx, time.Now().UTC())
			if b.Log != nil {
				if err != nil && ctx.Err() == nil {
					b.Log("broker: %v", err)
				}
				for _, s := range steps {
					if s.Action != "wake" {
						b.Log("broker: %s %s message %d", s.Action, s.ObligationID, s.MessageSeq)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
