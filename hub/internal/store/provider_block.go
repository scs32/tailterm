package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Provider blocks (docs/provider-blocked.md). An episode is one run of
// provider failures of one class for one agent run. The item lead hears of a
// new episode at once; the owner hears after a grace period, so agents of one
// runtime that block together become a single outage notice.

func migrateProviderBlock(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS provider_block_episodes (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL,
 runtime TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, class TEXT NOT NULL,
 code TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 0, since TEXT NOT NULL,
 opened_at TEXT NOT NULL, cleared_at TEXT NOT NULL DEFAULT '', lead_notice_seq INTEGER NOT NULL DEFAULT 0,
 owner_state TEXT NOT NULL DEFAULT 'pending', owner_notice_seq INTEGER NOT NULL DEFAULT 0,
 outage_id TEXT NOT NULL DEFAULT '', UNIQUE(agent_id,run_id,class,since));
CREATE INDEX IF NOT EXISTS provider_block_episodes_pending ON provider_block_episodes(owner_state,task_id,runtime);
CREATE TABLE IF NOT EXISTS provider_outages (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, runtime TEXT NOT NULL, provider TEXT NOT NULL,
 opened_at TEXT NOT NULL, closed_at TEXT NOT NULL DEFAULT '', notice_seq INTEGER NOT NULL DEFAULT 0,
 agent_count INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS provider_outages_open ON provider_outages(task_id,runtime) WHERE closed_at='';`)
	return err
}

const (
	providerBlockLeadSubject   = "An agent is blocked by its provider"
	providerBlockOwnerSubject  = "An agent is blocked by its provider and needs the owner"
	providerBlockOutageSubject = "Provider outage is blocking every active agent of one runtime"

	providerOwnerPending = "pending"
	providerOwnerAgent   = "agent"
	providerOwnerOutage  = "outage"

	liveAgentFilter = `a.status NOT IN ('closed','exited','retired')`
)

// providerBlockGrace is how long the owner notice waits for peers of the same
// runtime to block too.
func providerBlockGrace() time.Duration {
	v, err := strconv.Atoi(os.Getenv("TAILTERM_PROVIDER_BLOCK_GRACE"))
	if err != nil || v < 1 || v > 3600 {
		v = 90
	}
	return time.Duration(v) * time.Second
}

// providerBlockRecovery is the manual recovery for a class (the table in
// docs/provider-blocked.md). Nothing is switched or typed automatically.
func providerBlockRecovery(runtime, class string) string {
	switch class {
	case api.ProviderBlockUsageLimit:
		if runtime == "codex" {
			return "answer the usage-limit prompt in the agent's terminal"
		}
		return "wait for the reset, or run /model in the agent's terminal to pick a model with quota, or /usage-credits"
	case api.ProviderBlockAuth:
		if runtime == "codex" {
			return "run codex login on the host, then resume the agent"
		}
		return "run /login in the agent's terminal"
	case api.ProviderBlockRateLimited:
		return "wait and prompt the agent again; run fewer agents in parallel if it repeats"
	}
	return "check the provider's status and prompt the agent again when it recovers"
}

func providerBlockText(p api.ProviderBlock) string {
	text := fmt.Sprintf("%s %s", p.Provider, p.Class)
	var detail []string
	if p.Code != "" {
		detail = append(detail, "code "+p.Code)
	}
	if p.Status != 0 {
		detail = append(detail, fmt.Sprintf("status %d", p.Status))
	}
	if len(detail) > 0 {
		text += " (" + strings.Join(detail, ", ") + ")"
	}
	return fmt.Sprintf("%s, model %s, since %s", text, p.Model, p.Since.UTC().Format(time.RFC3339))
}

// providerBlockHook runs in the activity report transaction. A provider_blocked
// report opens an episode and tells the item lead once; replays, flaps back to
// the same class and since, and relay restarts find the row and post nothing.
// A report in any other state clears the run's open episodes.
func (s *Store) providerBlockHook(ctx context.Context, tx *sql.Tx, task, agent, run string, activity api.AgentActivity) error {
	now := ts(s.now())
	p := activity.Provider
	if activity.State != "provider_blocked" || p == nil {
		_, err := tx.ExecContext(ctx, `UPDATE provider_block_episodes SET cleared_at=? WHERE agent_id=? AND run_id=? AND cleared_at=''`, now, agent, run)
		return err
	}
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, agent))
	if err != nil {
		return err
	}
	// The owner helper is the owner's own session; the owner already sees it.
	if a.Role == api.AgentRoleOwnerHelper {
		return nil
	}
	since := ts(p.Since)
	// A change of class or since ends the earlier episode of this run.
	if _, err := tx.ExecContext(ctx, `UPDATE provider_block_episodes SET cleared_at=? WHERE agent_id=? AND run_id=? AND cleared_at='' AND NOT (class=? AND since=?)`, now, agent, run, p.Class, since); err != nil {
		return err
	}
	id := api.NewID("pblk")
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO provider_block_episodes(id,task_id,agent_id,run_id,runtime,provider,model,class,code,status,since,opened_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, task, agent, run, p.Runtime, p.Provider, p.Model, p.Class, p.Code, p.Status, since, now)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		// The same block again after a flap: open, with no second notice.
		_, err = tx.ExecContext(ctx, `UPDATE provider_block_episodes SET cleared_at='' WHERE agent_id=? AND run_id=? AND class=? AND since=?`, agent, run, p.Class, since)
		return err
	}
	var item, leadID, leadRun string
	err = tx.QueryRowContext(ctx, `SELECT item_id FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, agent, run).Scan(&item)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if item == "" {
		return nil
	}
	err = tx.QueryRowContext(ctx, `SELECT agent_id,run_id FROM item_team_leads WHERE task_id=? AND item_id=? AND state<>'closed'`, task, item).Scan(&leadID, &leadRun)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if leadID == "" || leadID == agent {
		return nil
	}
	lead, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND run_id=?`, leadID, leadRun))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (lead.Status == api.AgentClosed || lead.Status == api.AgentExited)) {
		return nil
	}
	if err != nil {
		return err
	}
	project, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE tasks.id=?`, task))
	if err != nil {
		return err
	}
	refs := map[string]string{"agent": agent, "run": run, "activity": activity.State, "item": item, "provider": p.Provider, "providerClass": p.Class}
	text := fmt.Sprintf("%s is blocked by its provider: %s. Its turns fail until this is fixed; it is not stalled or crashed. Recovery: %s. Nothing is switched automatically (docs/provider-blocked.md).",
		a.Name, providerBlockText(*p), providerBlockRecovery(p.Runtime, p.Class))
	if err := s.postBrokerNotice(ctx, tx, project, lead, lead.Name, providerBlockLeadSubject, providerBlockLeadSubject, text, refs); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE provider_block_episodes SET lead_notice_seq=(SELECT max(seq) FROM messages WHERE task_id=?) WHERE id=?`, task, id)
	return err
}

type providerEpisode struct {
	id, agentID, runID, name string
	block                    api.ProviderBlock
	openedAt                 time.Time
	cleared, live            bool
}

// ProviderBlockSweep sends the owner notices that waited for the grace
// period: one per project and runtime when it is an outage (two or more live
// agents of the runtime blocked and none working), otherwise one per episode.
// An open outage ends as soon as that condition stops holding.
// It runs for paused projects too and returns the number of notices posted.
func (s *Store) ProviderBlockSweep(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT e.task_id,e.runtime FROM provider_block_episodes e JOIN tasks t ON t.id=e.task_id WHERE e.owner_state=? AND t.status=?
 UNION SELECT o.task_id,o.runtime FROM provider_outages o JOIN tasks t ON t.id=o.task_id WHERE o.closed_at='' AND t.status=?`, providerOwnerPending, api.TaskOpen, api.TaskOpen)
	if err != nil {
		return 0, err
	}
	type scope struct{ task, runtime string }
	var scopes []scope
	for rows.Next() {
		var sc scope
		if err := rows.Scan(&sc.task, &sc.runtime); err != nil {
			rows.Close()
			return 0, err
		}
		scopes = append(scopes, sc)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	posted := 0
	var firstErr error
	for _, sc := range scopes {
		n, err := s.sweepProviderScope(ctx, sc.task, sc.runtime, now)
		posted += n
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("provider block sweep %s %s: %w", sc.task, sc.runtime, err)
		}
	}
	return posted, firstErr
}

func (s *Store) sweepProviderScope(ctx context.Context, task, runtime string, now time.Time) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.agent_id,e.run_id,a.name,e.provider,e.model,e.class,e.code,e.status,e.since,e.opened_at,e.cleared_at<>'',
 a.run_id=e.run_id AND `+liveAgentFilter+` FROM provider_block_episodes e JOIN agents a ON a.id=e.agent_id
 WHERE e.task_id=? AND e.runtime=? AND e.owner_state=? ORDER BY e.opened_at,e.id`, task, runtime, providerOwnerPending)
	if err != nil {
		return 0, err
	}
	var pending []providerEpisode
	for rows.Next() {
		e := providerEpisode{block: api.ProviderBlock{Runtime: runtime}}
		var since, opened string
		if err := rows.Scan(&e.id, &e.agentID, &e.runID, &e.name, &e.block.Provider, &e.block.Model, &e.block.Class, &e.block.Code, &e.block.Status, &since, &opened, &e.cleared, &e.live); err != nil {
			rows.Close()
			return 0, err
		}
		e.block.Since, e.openedAt = parseTS(since), parseTS(opened)
		pending = append(pending, e)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	// Blocked: live agents of the runtime with an open episode, whatever its
	// owner state. Working: live agents of the runtime reported working.
	var blocked, working int
	if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT e.agent_id) FROM provider_block_episodes e JOIN agents a ON a.id=e.agent_id AND a.run_id=e.run_id
 WHERE e.task_id=? AND e.runtime=? AND e.cleared_at='' AND `+liveAgentFilter, task, runtime).Scan(&blocked); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents a JOIN agent_activity x ON x.agent_id=a.id AND x.run_id=a.run_id
 WHERE a.task_id=? AND a.runtime=? AND x.state='working' AND `+liveAgentFilter, task, runtime).Scan(&working); err != nil {
		return 0, err
	}
	var outage string
	err = tx.QueryRowContext(ctx, `SELECT id FROM provider_outages WHERE task_id=? AND runtime=? AND closed_at=''`, task, runtime).Scan(&outage)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	attach := func(id string) error {
		for _, e := range pending {
			if _, err := tx.ExecContext(ctx, `UPDATE provider_block_episodes SET owner_state=?,outage_id=? WHERE id=?`, providerOwnerOutage, id, e.id); err != nil {
				return err
			}
		}
		return nil
	}
	posted := 0
	due := 0
	for _, e := range pending {
		if !now.Before(e.openedAt.Add(providerBlockGrace())) {
			due++
		}
	}
	if outage != "" {
		// An outage lasts only while its condition holds. It ends when no live
		// agent of the runtime is blocked, when one of the runtime is working,
		// or when an agent in it has recovered (a completed or running turn; a
		// runtime prompt, an unknown probe or a crash is not recovery). Blocks
		// that open afterwards are judged afresh below, so a stale outage never
		// swallows a later owner notice.
		var recovered int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM provider_block_episodes e JOIN agents a ON a.id=e.agent_id AND a.run_id=e.run_id
 JOIN agent_activity x ON x.agent_id=a.id AND x.run_id=a.run_id
 WHERE e.outage_id=? AND x.state IN ('working','idle','finished_silent') AND `+liveAgentFilter, outage).Scan(&recovered); err != nil {
			return 0, err
		}
		if blocked == 0 || working > 0 || recovered > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE provider_outages SET closed_at=? WHERE id=?`, ts(now), outage); err != nil {
				return 0, err
			}
			outage = ""
		}
	}
	switch {
	case outage != "":
		// Agents that block while the outage holds join it without a notice.
		if err := attach(outage); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE provider_outages SET agent_count=max(agent_count,?) WHERE id=?`, blocked, outage); err != nil {
			return 0, err
		}
	case due == 0:
		return 0, tx.Commit()
	default:
		project, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE tasks.id=?`, task))
		if err != nil {
			return 0, err
		}
		if blocked >= 2 && working == 0 {
			outage = api.NewID("pout")
			// Name every blocked live agent, including one the owner already
			// heard about while a peer was still working.
			classes := map[string]bool{}
			names := map[string]bool{}
			var earliest time.Time
			open, err := tx.QueryContext(ctx, `SELECT a.name,e.class,e.since FROM provider_block_episodes e JOIN agents a ON a.id=e.agent_id AND a.run_id=e.run_id
 WHERE e.task_id=? AND e.runtime=? AND e.cleared_at='' AND `+liveAgentFilter, task, runtime)
			if err != nil {
				return 0, err
			}
			for open.Next() {
				var name, class, since string
				if err := open.Scan(&name, &class, &since); err != nil {
					open.Close()
					return 0, err
				}
				classes[class], names[name] = true, true
				if at := parseTS(since); earliest.IsZero() || at.Before(earliest) {
					earliest = at
				}
			}
			if err := open.Close(); err != nil {
				return 0, err
			}
			provider := pending[0].block.Provider
			text := fmt.Sprintf("Provider outage for runtime %s (%s): %d agents are blocked and none of that runtime is working. Classes: %s. Earliest failure %s. Agents: %s. This is one notice for the runtime; agents that block while it lasts are added without another. Recovery per class is in docs/provider-blocked.md; nothing is switched automatically.",
				runtime, provider, blocked, strings.Join(sortedKeys(classes), ", "), earliest.UTC().Format(time.RFC3339), strings.Join(sortedKeys(names), ", "))
			refs := map[string]string{"activity": "provider_blocked", "runtime": runtime, "provider": provider, "outage": outage, "recipientKind": "owner"}
			if err := s.postBrokerNotice(ctx, tx, project, api.Agent{}, "", providerBlockOutageSubject, providerBlockOutageSubject, text, refs); err != nil {
				return 0, err
			}
			posted++
			if _, err := tx.ExecContext(ctx, `INSERT INTO provider_outages(id,task_id,runtime,provider,opened_at,notice_seq,agent_count) VALUES(?,?,?,?,?,(SELECT max(seq) FROM messages WHERE task_id=?),?)`,
				outage, task, runtime, provider, ts(now), task, blocked); err != nil {
				return 0, err
			}
			if err := attach(outage); err != nil {
				return 0, err
			}
			break
		}
		for _, e := range pending {
			if now.Before(e.openedAt.Add(providerBlockGrace())) {
				continue
			}
			text := fmt.Sprintf("%s is blocked by its provider: %s.", e.name, providerBlockText(e.block))
			switch {
			case e.cleared:
				text = fmt.Sprintf("%s was blocked by its provider: %s. The block has already cleared; no action is needed.", e.name, providerBlockText(e.block))
			case !e.live:
				text += " That run has since ended."
			default:
				text += fmt.Sprintf(" Its turns fail until this is fixed. Recovery: %s. Nothing is switched automatically (docs/provider-blocked.md).", providerBlockRecovery(runtime, e.block.Class))
			}
			refs := map[string]string{"agent": e.agentID, "run": e.runID, "activity": "provider_blocked", "provider": e.block.Provider, "providerClass": e.block.Class, "recipientKind": "owner"}
			if err := s.postBrokerNotice(ctx, tx, project, api.Agent{}, "", providerBlockOwnerSubject, providerBlockOwnerSubject, text, refs); err != nil {
				return 0, err
			}
			posted++
			if _, err := tx.ExecContext(ctx, `UPDATE provider_block_episodes SET owner_state=?,owner_notice_seq=(SELECT max(seq) FROM messages WHERE task_id=?) WHERE id=?`, providerOwnerAgent, task, e.id); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if posted > 0 {
		s.notify(task)
	}
	return posted, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
