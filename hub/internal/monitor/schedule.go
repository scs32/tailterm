// Package monitor contains the deliberately narrow, deterministic schedule
// enforcer. It observes durable queue/agent state and can only send a directed
// lead notification; it has no Queue or lifecycle mutation capability.
package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

const notificationText = "GO! Schedule monitor detected an actionable Queue stall (%s). Review the Queue and worker state. This is a notification only: it does not approve, claim, start, reassign, retire, close, or otherwise mutate task work."

type Config struct {
	Interval        time.Duration
	StallAfter      time.Duration
	WorkerSilence   time.Duration
	InitialBackoff  time.Duration
	MaxBackoff      time.Duration
	NoticeRetention time.Duration
}

func DefaultConfig() Config {
	return Config{Interval: 30 * time.Second, StallAfter: 5 * time.Minute, WorkerSilence: 2 * time.Minute, InitialBackoff: 5 * time.Minute, MaxBackoff: time.Hour, NoticeRetention: 7 * 24 * time.Hour}
}

func (c Config) valid() bool {
	return c.Interval > 0 && c.StallAfter > 0 && c.WorkerSilence > 0 && c.InitialBackoff > 0 && c.MaxBackoff >= c.InitialBackoff && c.NoticeRetention > 0
}

type Outcome struct {
	TaskID      string
	Fingerprint string
	State       string // notified, suppressed, no_work, intentional_block, unknown, delivery_failed
	MessageSeq  int64
	Err         error
	Log         bool // true only for a changed, nonquiet Tick outcome
}

type Enforcer struct {
	store        *store.Store
	config       Config
	now          func() time.Time
	tickMu       sync.Mutex
	lastOutcomes map[string]string
}

func New(st *store.Store, config Config) (*Enforcer, error) {
	if st == nil || !config.valid() {
		return nil, fmt.Errorf("invalid schedule monitor configuration")
	}
	return &Enforcer{store: st, config: config, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Start returns a completion signal so shutdown can wait before closing storage.
func (e *Enforcer) Start(ctx context.Context, report func(Outcome)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(e.config.Interval)
		defer ticker.Stop()
		e.run(ctx, ticker.C, report)
	}()
	return done
}

// run uses the same serial loop in production and deterministic channel tests.
func (e *Enforcer) run(ctx context.Context, ticks <-chan time.Time, report func(Outcome)) {
	if ctx.Err() != nil {
		return
	}
	e.Tick(ctx, report)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok || ctx.Err() != nil {
				return
			}
			e.Tick(ctx, report)
		}
	}
}

// Tick is exposed for deterministic tests and controlled embedding. An
// observation error is fail-closed: it produces an unknown outcome and sends
// no notification.
func (e *Enforcer) Tick(ctx context.Context, report func(Outcome)) {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	next := map[string]string{}
	sink := report
	report = func(out Outcome) {
		summary := out.Summary()
		out.Log = out.State != "no_work" && out.State != "suppressed" && e.lastOutcomes[out.TaskID] != summary
		next[out.TaskID] = summary
		if sink != nil {
			sink(out)
		}
	}
	// Only this tick's tasks remain, bounding suppression memory by current tasks.
	defer func() { e.lastOutcomes = next }()

	if _, err := e.store.PruneScheduleMonitorNotices(ctx, e.now().UTC().Add(-e.config.NoticeRetention)); err != nil {
		report(Outcome{State: "unknown", Err: fmt.Errorf("prune monitor notices: %w", err)})
		return // fail closed when durable retry metadata cannot be maintained
	}
	tasks, err := e.store.ListTasks(ctx)
	if err != nil {
		report(Outcome{State: "unknown", Err: err})
		return
	}
	for _, task := range tasks {
		if task.Status != api.TaskOpen {
			continue
		}
		e.tickTask(ctx, task, report)
	}
}

func (e *Enforcer) tickTask(ctx context.Context, task api.Task, report func(Outcome)) {
	agents, err := e.store.ListAgents(ctx, task.ID)
	if err != nil {
		report(Outcome{TaskID: task.ID, State: "unknown", Err: err})
		return
	}
	lead, agentByID, ok := exactLead(task, agents)
	if !ok {
		report(Outcome{TaskID: task.ID, State: "unknown", Err: fmt.Errorf("orchestrator identity is absent or ambiguous")})
		return
	}
	if lead.Status == api.AgentRetired || lead.Status == api.AgentNeedsInput || lead.Status == api.AgentClosed || lead.Status == api.AgentExited {
		report(Outcome{TaskID: task.ID, State: "intentional_block"})
		return
	}
	entries, err := e.queue(ctx, task.ID)
	if err != nil {
		report(Outcome{TaskID: task.ID, State: "unknown", Err: err})
		return
	}
	if len(entries) == 0 {
		report(Outcome{TaskID: task.ID, State: "no_work"})
		return
	}
	now := e.now().UTC()
	candidate, intentional, unknown := e.detect(entries, agentByID, now)
	if unknown {
		report(Outcome{TaskID: task.ID, State: "unknown", Err: fmt.Errorf("Queue worker state is incomplete")})
		return
	}
	if intentional {
		report(Outcome{TaskID: task.ID, State: "intentional_block"})
		return
	}
	if candidate == "" {
		report(Outcome{TaskID: task.ID, State: "suppressed"})
		return
	}
	e.deliver(ctx, task.ID, lead, candidate, now, report)
}

func (e *Enforcer) queue(ctx context.Context, taskID string) ([]api.QueueEntry, error) {
	var out []api.QueueEntry
	cursor := ""
	for {
		page, err := e.store.ListQueue(ctx, taskID, cursor, api.MaxQueuePage, false)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Entries...)
		if page.Complete {
			return out, nil
		}
		cursor = page.Cursor
		if cursor == "" {
			return nil, fmt.Errorf("Queue page did not advance")
		}
	}
}

func exactLead(task api.Task, agents []api.Agent) (api.Agent, map[string]api.Agent, bool) {
	byID := make(map[string]api.Agent, len(agents))
	var matches []api.Agent
	for _, agent := range agents {
		byID[agent.ID] = agent
		if agent.Name == task.Orchestrator && agent.Role == "" {
			matches = append(matches, agent)
		}
	}
	if len(matches) != 1 {
		return api.Agent{}, byID, false
	}
	return matches[0], byID, true
}

func (e *Enforcer) detect(entries []api.QueueEntry, agents map[string]api.Agent, now time.Time) (candidate string, intentional, unknown bool) {
	// Sort makes notification choice independent of storage/page ordering.
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for _, entry := range entries {
		age := now.Sub(entry.UpdatedAt)
		if entry.UpdatedAt.IsZero() || age < 0 {
			unknown = true
			continue
		}
		switch entry.State {
		case api.QueueStateWaiting:
			if entry.Eligible && !entry.Stale && !entry.ReviewNeeded && !entry.PendingUpdate && age >= e.config.StallAfter {
				if candidate == "" {
					candidate = fingerprint("waiting", entry)
				}
			}
		case api.QueueStateClaimed:
			if entry.ReconciliationNeeded {
				continue // explicit unknown/reconciliation remains human-directed, never automated.
			}
			if age >= e.config.StallAfter {
				if candidate == "" {
					candidate = fingerprint("claimed", entry)
				}
			}
		case api.QueueStateActive:
			worker, found := agents[entry.WorkerAgentID]
			if entry.WorkerAgentID == "" || entry.WorkerRunID == "" || !found || worker.RunID != entry.WorkerRunID {
				if age >= e.config.StallAfter {
					if candidate == "" {
						candidate = fingerprint("worker_missing", entry)
					}
				}
				continue
			}
			if worker.Status == api.AgentNeedsInput || worker.Status == api.AgentRetired {
				intentional = true
				continue
			}
			if worker.Status == api.AgentClosed || worker.Status == api.AgentExited {
				if age >= e.config.StallAfter {
					if candidate == "" {
						candidate = fingerprint("worker_unavailable", entry)
					}
				}
				continue
			}
			if worker.LastSeenAt.IsZero() {
				unknown = true
				continue
			}
			if now.Sub(worker.LastSeenAt) >= e.config.WorkerSilence && age >= e.config.StallAfter {
				if candidate == "" {
					candidate = fingerprint("worker_silent", entry)
				}
			} else if age >= e.config.StallAfter && candidate == "" {
				// Queue state is durable progress evidence. A fresh heartbeat only
				// says the process lives; it does not prove the active work advances.
				candidate = fingerprint("active_stale", entry)
			}
		}
	}
	if candidate != "" {
		return candidate, false, false
	}
	if unknown {
		return "", false, true
	}
	if intentional {
		return "", true, false
	}
	return "", false, false
}

func fingerprint(kind string, entry api.QueueEntry) string {
	return fmt.Sprintf("%s:%s:%d:%d:%s", kind, entry.ID, entry.Cycle, entry.Revision, entry.WorkerRunID)
}

func (e *Enforcer) deliver(ctx context.Context, taskID string, lead api.Agent, fingerprint string, now time.Time, report func(Outcome)) {
	notice, state, err := e.store.ScheduleMonitorDelivery(ctx, taskID, lead, fingerprint, fmt.Sprintf(notificationText, fingerprint), now, e.config.InitialBackoff, e.config.MaxBackoff)
	report(Outcome{TaskID: taskID, Fingerprint: fingerprint, State: state, MessageSeq: notice.MessageSeq, Err: err})
}

func (e *Enforcer) backoff(count int) time.Duration {
	return store.ScheduleMonitorBackoff(e.config.InitialBackoff, e.config.MaxBackoff, count)
}

// Summary is intentionally stable for logs and tests without exposing any
// private context or credentials.
func (o Outcome) Summary() string {
	parts := []string{"schedule-monitor", o.State}
	if o.TaskID != "" {
		parts = append(parts, o.TaskID)
	}
	if o.Fingerprint != "" {
		parts = append(parts, o.Fingerprint)
	}
	if o.MessageSeq != 0 {
		parts = append(parts, fmt.Sprintf("message=%d", o.MessageSeq))
	}
	if o.Err != nil {
		parts = append(parts, "error="+o.Err.Error())
	}
	return strings.Join(parts, " ")
}
