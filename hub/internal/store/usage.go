package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

func migrateUsage(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS usage_runs(
 task_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,session TEXT NOT NULL,
 started_at TEXT NOT NULL,coverage TEXT NOT NULL,provenance TEXT NOT NULL,
 PRIMARY KEY(task_id,agent_id,run_id));
 CREATE TABLE IF NOT EXISTS usage_turns(
 task_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,request_id TEXT NOT NULL,
 revision INTEGER NOT NULL,at TEXT NOT NULL,payload TEXT NOT NULL,projection TEXT NOT NULL,
 PRIMARY KEY(task_id,agent_id,run_id,request_id));
 CREATE INDEX IF NOT EXISTS usage_turns_time ON usage_turns(task_id,at);
 CREATE TABLE IF NOT EXISTS usage_turn_revisions(
 task_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,request_id TEXT NOT NULL,
 revision INTEGER NOT NULL,payload TEXT NOT NULL,projection TEXT NOT NULL,
 PRIMARY KEY(task_id,agent_id,run_id,request_id,revision));
 CREATE TABLE IF NOT EXISTS usage_receipts(
 task_id TEXT NOT NULL,request_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,
 payload TEXT NOT NULL,receipt TEXT NOT NULL,PRIMARY KEY(task_id,request_id));
 CREATE TABLE IF NOT EXISTS usage_price_revisions(
 task_id TEXT NOT NULL,revision INTEGER NOT NULL,payload TEXT NOT NULL,
 PRIMARY KEY(task_id,revision));
 CREATE TABLE IF NOT EXISTS usage_price_receipts(
 task_id TEXT NOT NULL,request_id TEXT NOT NULL,payload TEXT NOT NULL,result TEXT NOT NULL,
 PRIMARY KEY(task_id,request_id));
 CREATE TABLE IF NOT EXISTS usage_budget_warnings(
 task_id TEXT NOT NULL,item_id TEXT NOT NULL,estimate_tokens INTEGER NOT NULL,threshold TEXT NOT NULL,
 actual_tokens TEXT NOT NULL,actual_state TEXT NOT NULL,lead_agent TEXT NOT NULL DEFAULT '',
 lead_message_seq INTEGER NOT NULL DEFAULT 0,helper_agent TEXT NOT NULL DEFAULT '',
 helper_message_seq INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,
 PRIMARY KEY(task_id,item_id,estimate_tokens));
 CREATE TABLE IF NOT EXISTS usage_entry_warnings(
 task_id TEXT NOT NULL,item_id TEXT NOT NULL,entry_id TEXT NOT NULL,estimate_tokens INTEGER NOT NULL,
 threshold TEXT NOT NULL,team_tokens TEXT NOT NULL,team_state TEXT NOT NULL,
 lifetime_tokens TEXT NOT NULL,lifetime_state TEXT NOT NULL,
 lead_agent TEXT NOT NULL DEFAULT '',lead_message_seq INTEGER NOT NULL DEFAULT 0,
 helper_agent TEXT NOT NULL DEFAULT '',helper_message_seq INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,
 PRIMARY KEY(task_id,item_id,entry_id,estimate_tokens));
 CREATE TABLE IF NOT EXISTS usage_warning_settings(
 task_id TEXT PRIMARY KEY,threshold TEXT NOT NULL,updated_at TEXT NOT NULL,
 by_agent TEXT NOT NULL DEFAULT '',by_node TEXT NOT NULL DEFAULT '',by_user TEXT NOT NULL DEFAULT '');
 ` + usageSpansSchema)
	return err
}
func validUsageTurn(t api.UsageTurn) bool {
	if !validRequestID(t.ID) || t.Revision < 1 || t.At.IsZero() || t.Session == "" || len(t.Session) > 128 || len(t.Model) > 160 || len(t.SourceDigest) != 64 || len(t.Activation) > 160 || len(t.Gap) > 240 || len(t.Handled) > 64 {
		return false
	}
	if t.Runtime != "codex" && t.Runtime != "claude" {
		return false
	}
	if _, err := hex.DecodeString(t.SourceDigest); err != nil {
		return false
	}
	normalized, _ := api.NormalizeUsageTokens(t.Runtime, t.Raw)
	if !reflect.DeepEqual(normalized, t.Tokens) {
		return false
	}
	for k, v := range t.Tokens {
		if v < 0 || v > 1e15 || !usageClass(k) {
			return false
		}
	}
	for k, v := range t.Raw {
		if len(k) > 80 || v < 0 || v > 1e15 {
			return false
		}
	}
	for _, e := range t.Handled {
		if !api.ValidID(e.TaskID, "tsk") || e.Seq < 1 || e.At.IsZero() || (e.Operation != "ack" && e.Operation != "progress" && e.Operation != "reply" && e.Operation != "post") {
			return false
		}
	}
	return len(t.Tokens) > 0
}
func usageClass(k string) bool {
	for _, x := range api.UsageClasses {
		if k == x {
			return true
		}
	}
	return false
}
func usageUpdate(old, t api.UsageTurn) bool {
	if t.Revision != old.Revision+1 || t.ID != old.ID || t.Runtime != old.Runtime || t.Session != old.Session || t.Model != old.Model || !t.At.Equal(old.At) || t.Activation != old.Activation {
		return false
	}
	for k := range old.Tokens {
		_, ok := t.Tokens[k]
		if !ok {
			return false
		}
	}
	for k, v := range old.Raw {
		n, ok := t.Raw[k]
		if !ok || n < v {
			return false
		}
	}
	_, oldNormalizationGap := api.NormalizeUsageTokens(old.Runtime, old.Raw)
	_, newNormalizationGap := api.NormalizeUsageTokens(t.Runtime, t.Raw)
	gapResolved := old.Gap == oldNormalizationGap && t.Gap == newNormalizationGap
	if old.Complete && (!t.Complete || !reflect.DeepEqual(old.Handled, t.Handled) || (old.Gap != t.Gap && !gapResolved)) {
		return false
	}
	return !reflect.DeepEqual(old.Tokens, t.Tokens) || !reflect.DeepEqual(old.Raw, t.Raw) || !reflect.DeepEqual(old.Handled, t.Handled) || old.Complete != t.Complete || old.Gap != t.Gap
}

// ReportUsage is independent of lifecycle/activity writes. Frozen enrolled runs
// remain ingestible after closure; an unobserved stale run cannot enroll.
func (s *Store) ReportUsage(ctx context.Context, task, agent string, b api.UsageBatch) (api.UsageReceipt, error) {
	zero := api.UsageReceipt{}
	raw, _ := json.Marshal(b)
	if !api.ValidID(task, "tsk") || !api.ValidID(agent, "agt") || b.Version != api.UsageVersion || !validRequestID(b.RequestID) || b.RunID == "" || b.Session == "" || b.StartedAt.IsZero() || len(b.Turns)+len(b.Spans) > 64 || len(raw) > 60<<10 || len(b.Coverage) > 240 {
		return zero, api.ErrInvalid
	}
	for _, span := range b.Spans {
		if !validUsageSpan(span) {
			return zero, api.ErrInvalid
		}
	}
	for _, t := range b.Turns {
		if !validUsageTurn(t) || t.Session != b.Session {
			return zero, api.ErrInvalid
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var prior, receipt, priorAgent, priorRun string
	err = tx.QueryRowContext(ctx, `SELECT payload,receipt,agent_id,run_id FROM usage_receipts WHERE task_id=? AND request_id=?`, task, b.RequestID).Scan(&prior, &receipt, &priorAgent, &priorRun)
	if err == nil {
		if !sameUsageBatch(prior, raw) || priorAgent != agent || priorRun != b.RunID {
			return zero, api.ErrConflict
		}
		err = json.Unmarshal([]byte(receipt), &zero)
		return zero, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	var frozen string
	err = tx.QueryRowContext(ctx, `SELECT provenance FROM usage_runs WHERE task_id=? AND agent_id=? AND run_id=? AND session=?`, task, agent, b.RunID, b.Session).Scan(&frozen)
	if errors.Is(err, sql.ErrNoRows) {
		a, loadErr := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, task, agent))
		if loadErr != nil || a.RunID != b.RunID {
			return zero, api.ErrConflict
		}
		binding, loadErr := loadAgentWorkItemBinding(tx, ctx, agent, b.RunID)
		if loadErr != nil {
			return zero, loadErr
		}
		provenance := usageRunProvenance{Agent: a, Binding: binding}
		if err = freezeUsageRole(ctx, tx, &provenance); err != nil {
			return zero, err
		}
		encoded, _ := json.Marshal(provenance)
		frozen = string(encoded)
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_runs VALUES(?,?,?,?,?,?,?)`, task, agent, b.RunID, b.Session, ts(b.StartedAt), b.Coverage, frozen)
	}
	if err != nil {
		return zero, err
	}
	var provenance usageRunProvenance
	if json.Unmarshal([]byte(frozen), &provenance) != nil {
		return zero, api.ErrConflict
	}
	// Items whose attributed usage this batch added or revised.
	touched := map[string]bool{}
	for _, t := range b.Turns {
		var oldRevision int64
		var oldPayload, oldProjection string
		err = tx.QueryRowContext(ctx, `SELECT revision,payload,projection FROM usage_turns WHERE task_id=? AND agent_id=? AND run_id=? AND request_id=?`, task, agent, b.RunID, t.ID).Scan(&oldRevision, &oldPayload, &oldProjection)
		var projection api.UsageProjection
		if err == nil {
			// A turn is stored once, in its projection; a row written before
			// that also holds it as the payload.
			var old api.UsageTurn
			if oldPayload == "" {
				var stored api.UsageProjection
				if json.Unmarshal([]byte(oldProjection), &stored) != nil {
					return zero, api.ErrConflict
				}
				old = stored.Turn
			} else if json.Unmarshal([]byte(oldPayload), &old) != nil {
				return zero, api.ErrConflict
			}
			if reflect.DeepEqual(old, t) {
				continue
			}
			if !usageUpdate(old, t) {
				return zero, api.ErrConflict
			}
			if json.Unmarshal([]byte(oldProjection), &projection) != nil {
				return zero, api.ErrConflict
			}
			// Only the revision being replaced is kept as history; one stored
			// by an earlier hub is already there.
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO usage_turn_revisions VALUES(?,?,?,?,?,?,?)`, task, agent, b.RunID, t.ID, oldRevision, oldPayload, oldProjection); err != nil {
				return zero, err
			}
			// Only explicitly provisional requests can finalize their handled set.
			if !reflect.DeepEqual(old.Handled, t.Handled) {
				projection, err = resolveUsage(ctx, tx, task, agent, b.RunID, t, provenance)
			} else {
				projection.Turn = t
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if t.Revision != 1 {
				return zero, api.ErrConflict
			}
			projection, err = resolveUsage(ctx, tx, task, agent, b.RunID, t, provenance)
		}
		if err != nil {
			return zero, err
		}
		encoded, _ := json.Marshal(projection)
		_, err = tx.ExecContext(ctx, `INSERT INTO usage_turns VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(task_id,agent_id,run_id,request_id) DO UPDATE SET revision=excluded.revision,at=excluded.at,payload=excluded.payload,projection=excluded.projection`, task, agent, b.RunID, t.ID, t.Revision, ts(t.At), "", string(encoded))
		if err != nil {
			return zero, err
		}
		if err = replaceUsageItemShares(ctx, tx, task, agent, b.RunID, projection); err != nil {
			return zero, err
		}
		for _, share := range projection.Shares {
			// Like the budget, only the project's own items.
			if share.ItemID != "" && share.TaskID == task {
				touched[share.ItemID] = true
			}
		}
	}
	warned, err := s.warnTokenBudgets(ctx, tx, task, touched)
	if err != nil {
		return zero, err
	}
	if err = storeUsageSpans(ctx, tx, task, agent, b.RunID, b.Spans, provenance); err != nil {
		return zero, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE usage_runs SET coverage=? WHERE task_id=? AND agent_id=? AND run_id=?`, b.Coverage, task, agent, b.RunID)
	if err != nil {
		return zero, err
	}
	zero = api.UsageReceipt{RequestID: b.RequestID, Turns: len(b.Turns), Spans: len(b.Spans)}
	encoded, _ := json.Marshal(zero)
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_receipts VALUES(?,?,?,?,?,?)`, task, b.RequestID, agent, b.RunID, usageBatchHash(raw), string(encoded))
	if err != nil {
		return api.UsageReceipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.UsageReceipt{}, err
	}
	if warned {
		// A warning notice was committed: wake event waiters, as a post does.
		s.notify(task)
	}
	return zero, nil
}
func usageInvalid(reason string) error {
	return fmt.Errorf("%w: usage %s", api.ErrInvalid, strings.TrimSpace(reason))
}
