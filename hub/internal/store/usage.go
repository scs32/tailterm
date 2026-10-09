package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

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
 CREATE TABLE IF NOT EXISTS usage_turn_totals(
 task_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,request_id TEXT NOT NULL,
 turn_revision INTEGER NOT NULL,runtime TEXT NOT NULL,at_ns INTEGER NOT NULL,tokens INTEGER NOT NULL,
 PRIMARY KEY(task_id,agent_id,run_id,request_id));
 CREATE INDEX IF NOT EXISTS usage_turn_totals_window ON usage_turn_totals(task_id,runtime,at_ns,tokens);
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
 CREATE TABLE IF NOT EXISTS usage_budgets(
 task_id TEXT NOT NULL,runtime TEXT NOT NULL,window TEXT NOT NULL,
 allowance_tokens INTEGER NOT NULL CHECK(allowance_tokens>0),reserve_percent INTEGER NOT NULL,
 reset_at TEXT NOT NULL DEFAULT '',stale_seconds INTEGER NOT NULL DEFAULT 900,updated_at TEXT NOT NULL,
 by_agent TEXT NOT NULL DEFAULT '',by_node TEXT NOT NULL DEFAULT '',by_user TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(task_id,runtime,window));
 CREATE TABLE IF NOT EXISTS usage_provider_readings(
 host TEXT NOT NULL,runtime TEXT NOT NULL,window TEXT NOT NULL,state TEXT NOT NULL,
 used_percent TEXT NOT NULL DEFAULT '',resets_at TEXT NOT NULL DEFAULT '',captured_at TEXT NOT NULL DEFAULT '',
 reported_at TEXT NOT NULL,version TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(host,runtime,window));
 CREATE TABLE IF NOT EXISTS usage_estimate_defaults(
 task_id TEXT PRIMARY KEY,small_tokens INTEGER NOT NULL,small_race_tokens INTEGER NOT NULL,
 planned_tokens INTEGER NOT NULL,planned_race_tokens INTEGER NOT NULL,updated_at TEXT NOT NULL,
 by_agent TEXT NOT NULL DEFAULT '',by_node TEXT NOT NULL DEFAULT '',by_user TEXT NOT NULL DEFAULT '');
 ` + usageSpansSchema)
	if err != nil {
		return err
	}
	return backfillUsageTurnTotals(db)
}

// replaceUsageTurnTotal writes one stored turn's row in usage_turn_totals: its
// runtime, its instant as Unix nanoseconds and its reported token classes
// summed. The allowance source of the token budget sums these rows through
// usage_turn_totals_window instead of decoding every projection.
func replaceUsageTurnTotal(ctx context.Context, tx *sql.Tx, task, agent, run, request string, revision int64, runtime string, at time.Time, counts map[string]int64) error {
	var tokens int64
	for _, class := range api.UsageClasses {
		tokens += counts[class]
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_turn_totals(task_id,agent_id,run_id,request_id,turn_revision,runtime,at_ns,tokens) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(task_id,agent_id,run_id,request_id) DO UPDATE SET turn_revision=excluded.turn_revision,runtime=excluded.runtime,at_ns=excluded.at_ns,tokens=excluded.tokens`,
		task, agent, run, request, revision, runtime, at.UnixNano(), tokens)
	return err
}

// backfillUsageTurnTotals gives a totals row to every stored turn that has
// none at its current revision: turns stored before the table existed, and
// turns an older binary wrote or revised after a rollback. Like
// backfillUsageItemShares it is idempotent and works in bounded transactions,
// so an interrupted open resumes. A turn whose projection does not decode gets
// no row, as the allowance read skipped it before.
func backfillUsageTurnTotals(db *sql.DB) error {
	ctx := context.Background()
	type turn struct {
		rowid, revision                       int64
		task, agent, run, request, projection string
	}
	after := int64(0)
	for {
		rows, err := db.QueryContext(ctx, `SELECT t.rowid,t.revision,t.task_id,t.agent_id,t.run_id,t.request_id,t.projection FROM usage_turns t
 WHERE t.rowid>? AND NOT EXISTS (SELECT 1 FROM usage_turn_totals s WHERE s.task_id=t.task_id AND s.agent_id=t.agent_id AND s.run_id=t.run_id AND s.request_id=t.request_id AND s.turn_revision=t.revision)
 ORDER BY t.rowid LIMIT 500`, after)
		if err != nil {
			return err
		}
		batch := []turn{}
		for rows.Next() {
			var x turn
			if err = rows.Scan(&x.rowid, &x.revision, &x.task, &x.agent, &x.run, &x.request, &x.projection); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, x := range batch {
			after = x.rowid
			var p struct {
				Turn struct {
					Runtime string           `json:"runtime"`
					At      time.Time        `json:"at"`
					Tokens  map[string]int64 `json:"tokens"`
				} `json:"turn"`
			}
			if json.Unmarshal([]byte(x.projection), &p) != nil {
				continue
			}
			if err = replaceUsageTurnTotal(ctx, tx, x.task, x.agent, x.run, x.request, x.revision, p.Turn.Runtime, p.Turn.At, p.Turn.Tokens); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
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
		if err = replaceUsageTurnTotal(ctx, tx, task, agent, b.RunID, t.ID, t.Revision, projection.Turn.Runtime, projection.Turn.At, projection.Turn.Tokens); err != nil {
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
	held, err := s.holdTokenBudgets(ctx, tx, task, touched)
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
	if warned || held {
		// A warning notice or a hold with its ask was committed: wake event
		// waiters, as a post does.
		s.notify(task)
	}
	return zero, nil
}
func usageInvalid(reason string) error {
	return fmt.Errorf("%w: usage %s", api.ErrInvalid, strings.TrimSpace(reason))
}

// usageBudgetWindows lists the budget windows in a stable order.
var usageBudgetWindows = []string{api.UsageWindowFiveHour, api.UsageWindowSevenDay}

func usageBudgetRuntime(runtime string) bool { return runtime == "codex" || runtime == "claude" }

// usageProjectExists refuses an unknown project.
func usageProjectExists(ctx context.Context, q queryRower, task string) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id=?`, task).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return api.ErrNotFound
	}
	return nil
}

// usageSettingsWriter allows the owner (no agent identity), the project's
// owner helper and its database handlers to change what the token budget
// compares; any other agent is refused with a named reason.
func usageSettingsWriter(ctx context.Context, tx *sql.Tx, task, agent, what string) error {
	if agent == "" {
		return nil
	}
	helper, ok, err := currentOwnerHelper(ctx, tx, task)
	if err != nil {
		return err
	}
	if ok && helper.ID == agent {
		return nil
	}
	var role, status string
	err = tx.QueryRowContext(ctx, `SELECT role,status FROM agents WHERE task_id=? AND id=?`, task, agent).Scan(&role, &status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && role == api.AgentRoleDatabaseHandler && status != api.AgentClosed && status != api.AgentExited && status != api.AgentRetired {
		return nil
	}
	return workItemConflict("only the owner, the owner helper or a database handler sets " + what)
}

// usageBudgetRow is one stored budget row.
type usageBudgetRow struct {
	api.UsageBudget
	resetAt time.Time
}

func loadUsageBudgetRows(ctx context.Context, q queryRower, task string) ([]usageBudgetRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT runtime,window,allowance_tokens,reserve_percent,reset_at,stale_seconds,updated_at,by_agent,by_node,by_user FROM usage_budgets WHERE task_id=? ORDER BY runtime,window`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageBudgetRow{}
	for rows.Next() {
		var r usageBudgetRow
		if err := rows.Scan(&r.Runtime, &r.Window, &r.AllowanceTokens, &r.ReservePercent, &r.ResetAt, &r.StaleSeconds, &r.UpdatedAt, &r.UpdatedBy.AgentID, &r.UpdatedBy.Node, &r.UpdatedBy.User); err != nil {
			return nil, err
		}
		r.resetAt = parseTS(r.ResetAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// usageBudgetState is what admission uses for one budget row now.
type usageBudgetState struct {
	row       usageBudgetRow
	source    string
	remaining *big.Rat // nil when the source is none
	reset     time.Time
	// reading says why the provider reading is not the source; empty when it is.
	reading string
	// captured is the reading's capture instant when the source is the
	// provider: the reading reflects use only up to it.
	captured time.Time
}

// usageRuntimeTokensSince sums the project's reported tokens of one runtime in
// turns at or after an instant. It reads one sum from usage_turn_totals through
// the covering index usage_turn_totals_window: one index entry per turn in the
// window, no usage_turns row and no JSON. Instants are Unix nanoseconds, so the
// boundary is exact.
func usageRuntimeTokensSince(ctx context.Context, q queryRower, task, runtime string, since time.Time) (int64, error) {
	var total int64
	err := q.QueryRowContext(ctx, usageRuntimeTokensSinceSQL, task, runtime, since.UnixNano()).Scan(&total)
	return total, err
}

const usageRuntimeTokensSinceSQL = `SELECT COALESCE(sum(tokens),0) FROM usage_turn_totals WHERE task_id=? AND runtime=? AND at_ns>=?`

// usageBudgetStatus computes, for each budget row of the project, what
// admission on a host would use now. The first source that applies wins:
//
//  1. provider: the host's reading is ok, not older than the row's staleness
//     bound and its reset is still ahead. Remaining is the allowance times the
//     unused percentage.
//  2. allowance, reset from the reading: the reading is ok but stale, or its
//     reset has passed. Remaining is the allowance minus the project's
//     reported tokens of the runtime since the window start. A passed reset is
//     never counted as zero use.
//  3. allowance, owner reset: no ok reading and the row has a reset instant.
//  4. none: nothing to measure against; admission holds.
//
// Unknown is never treated as plenty.
func usageBudgetStatus(ctx context.Context, q queryRower, task, host string, now time.Time) ([]usageBudgetState, error) {
	rows, err := loadUsageBudgetRows(ctx, q, task)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]usageBudgetState, 0, len(rows))
	for _, row := range rows {
		state := usageBudgetState{row: row, source: api.UsageBudgetSourceNone}
		length := api.UsageWindowLength(row.Window)
		allowance := big.NewRat(row.AllowanceTokens, 1)
		var readingState, used, resets, captured string
		err := q.QueryRowContext(ctx, `SELECT state,used_percent,resets_at,captured_at FROM usage_provider_readings WHERE host=? AND runtime=? AND window=?`, host, row.Runtime, row.Window).Scan(&readingState, &used, &resets, &captured)
		if errors.Is(err, sql.ErrNoRows) {
			readingState, err = "not reported", nil
		}
		if err != nil {
			return nil, err
		}
		percent, percentOK := new(big.Rat).SetString(used)
		resetsAt, capturedAt := parseTS(resets), parseTS(captured)
		usable := readingState == api.ProviderUsageOK && percentOK && !resetsAt.IsZero() && !capturedAt.IsZero()
		var start time.Time
		switch {
		case usable && now.Sub(capturedAt) <= time.Duration(row.StaleSeconds)*time.Second && resetsAt.After(now):
			state.source, state.reset, state.captured = api.UsageBudgetSourceProvider, resetsAt, capturedAt
			unused := new(big.Rat).Sub(big.NewRat(100, 1), percent)
			state.remaining = unused.Mul(unused, allowance).Quo(unused, big.NewRat(100, 1))
		case usable:
			if resetsAt.After(now) {
				start, state.reset = resetsAt.Add(-length), resetsAt
				state.reading = fmt.Sprintf("is stale (captured %s)", capturedAt.UTC().Format(time.RFC3339))
			} else {
				start = resetsAt
				if floor := now.Add(-length); floor.After(start) {
					start = floor
				}
				state.reset = start.Add(length)
				state.reading = fmt.Sprintf("has a reset that passed at %s", resetsAt.UTC().Format(time.RFC3339))
			}
		case !row.resetAt.IsZero():
			// The latest reset_at + k windows that is not after now.
			k := now.Sub(row.resetAt) / length
			if now.Before(row.resetAt) && now.Sub(row.resetAt)%length != 0 {
				k--
			}
			start = row.resetAt.Add(k * length)
			state.reset = start.Add(length)
			state.reading = "is " + readingState
			if readingState == api.ProviderUsageOK {
				state.reading = "is malformed"
			}
		default:
			state.reading = "is " + readingState
			if readingState == api.ProviderUsageOK {
				state.reading = "is malformed"
			}
		}
		if !start.IsZero() {
			spent, err := usageRuntimeTokensSince(ctx, q, task, row.Runtime, start)
			if err != nil {
				return nil, err
			}
			state.source = api.UsageBudgetSourceAllowance
			state.remaining = new(big.Rat).Sub(allowance, big.NewRat(spent, 1))
		}
		out = append(out, state)
	}
	return out, nil
}

func readUsageBudgets(ctx context.Context, q queryRower, task, host string, now time.Time) (api.UsageBudgets, error) {
	out := api.UsageBudgets{Host: host, Budgets: []api.UsageBudget{}}
	if !api.ValidID(task, "tsk") || len(host) > 255 {
		return out, api.ErrInvalid
	}
	if err := usageProjectExists(ctx, q, task); err != nil {
		return out, err
	}
	states, err := usageBudgetStatus(ctx, q, task, host, now)
	if err != nil {
		return out, err
	}
	for _, state := range states {
		budget := state.row.UsageBudget
		status := api.UsageBudgetStatus{Host: host, Source: state.source, Reading: state.reading}
		if state.remaining != nil {
			status.RemainingTokens = state.remaining.RatString()
		}
		if !state.reset.IsZero() {
			status.ResetAt = ts(state.reset)
		}
		budget.Status = &status
		out.Budgets = append(out.Budgets, budget)
	}
	out.Configured = len(out.Budgets) > 0
	return out, nil
}

// UsageBudgets reads the project's budget rows with the source admission on
// the named host would use now. A project with no row is not configured.
func (s *Store) UsageBudgets(ctx context.Context, task, host string) (api.UsageBudgets, error) {
	return readUsageBudgets(ctx, s.db, task, host, s.now())
}

// minUsageBudgetStaleSeconds and maxUsageBudgetStaleSeconds bound a budget
// row's staleness setting. The floor is what the relay can keep fresh: it
// re-reports an unchanged reading every 5 minutes (providerUsageKeepFresh in
// cmd/tt/provider_usage.go) and retries a failed report after 1 minute
// (providerUsageRetry), so a smaller bound would make the reading stale most
// of the time. The store cannot import cmd/tt; keep the three in step.
const (
	minUsageBudgetStaleSeconds = 6 * 60
	maxUsageBudgetStaleSeconds = 24 * 60 * 60
)

// SetUsageBudget saves one budget row. The allowance is required: the
// estimate is always compared in tokens.
func (s *Store) SetUsageBudget(ctx context.Context, task string, req api.UsageBudgetRequest, by api.Caller) (api.UsageBudgets, error) {
	zero := api.UsageBudgets{}
	if !api.ValidID(task, "tsk") || (req.AgentID != "" && !api.ValidID(req.AgentID, "agt")) {
		return zero, api.ErrInvalid
	}
	if !usageBudgetRuntime(req.Runtime) {
		return zero, usageInvalid("budget runtime must be codex or claude")
	}
	if api.UsageWindowLength(req.Window) == 0 {
		return zero, usageInvalid("budget window must be five_hour or seven_day")
	}
	if req.AllowanceTokens < 1 || req.AllowanceTokens > 1e15 {
		return zero, usageInvalid("budget needs an allowance in tokens: what 100 percent of the window represents")
	}
	if req.ReservePercent < 0 || req.ReservePercent > 90 {
		return zero, usageInvalid("budget reserve must be 0 to 90 percent")
	}
	stale := req.StaleSeconds
	if stale == 0 {
		stale = api.DefaultUsageBudgetStaleSeconds
	}
	if stale < minUsageBudgetStaleSeconds || stale > maxUsageBudgetStaleSeconds {
		return zero, usageInvalid("budget staleness bound must be 6 minutes (360 seconds) to 24 hours: the relay re-reports an unchanged reading every 5 minutes and retries a failed report after 1 minute")
	}
	reset := ""
	if req.ResetAt != "" {
		at, err := time.Parse(time.RFC3339Nano, req.ResetAt)
		if err != nil {
			return zero, usageInvalid("budget reset time must be RFC 3339")
		}
		reset = ts(at)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err = usageProjectExists(ctx, tx, task); err != nil {
		return zero, err
	}
	if err = usageSettingsWriter(ctx, tx, task, req.AgentID, "the token budget"); err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_budgets(task_id,runtime,window,allowance_tokens,reserve_percent,reset_at,stale_seconds,updated_at,by_agent,by_node,by_user) VALUES(?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(task_id,runtime,window) DO UPDATE SET allowance_tokens=excluded.allowance_tokens,reserve_percent=excluded.reserve_percent,reset_at=excluded.reset_at,stale_seconds=excluded.stale_seconds,updated_at=excluded.updated_at,by_agent=excluded.by_agent,by_node=excluded.by_node,by_user=excluded.by_user`,
		task, req.Runtime, req.Window, req.AllowanceTokens, req.ReservePercent, reset, stale, ts(s.now()), req.AgentID, by.Node, by.User); err != nil {
		return zero, err
	}
	out, err := readUsageBudgets(ctx, tx, task, "", s.now())
	if err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	// A budget change can admit a held entry: wake the queue's waiters.
	s.notify(task)
	return out, nil
}

// DeleteUsageBudget clears one budget row; clearing the last one switches the
// budget check off for the project.
func (s *Store) DeleteUsageBudget(ctx context.Context, task string, req api.UsageBudgetRequest, by api.Caller) (api.UsageBudgets, error) {
	zero := api.UsageBudgets{}
	if !api.ValidID(task, "tsk") || (req.AgentID != "" && !api.ValidID(req.AgentID, "agt")) {
		return zero, api.ErrInvalid
	}
	if !usageBudgetRuntime(req.Runtime) || api.UsageWindowLength(req.Window) == 0 {
		return zero, usageInvalid("name the budget row to clear: runtime codex or claude, window five_hour or seven_day")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err = usageProjectExists(ctx, tx, task); err != nil {
		return zero, err
	}
	if err = usageSettingsWriter(ctx, tx, task, req.AgentID, "the token budget"); err != nil {
		return zero, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM usage_budgets WHERE task_id=? AND runtime=? AND window=?`, task, req.Runtime, req.Window)
	if err != nil {
		return zero, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return zero, api.ErrNotFound
	}
	out, err := readUsageBudgets(ctx, tx, task, "", s.now())
	if err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

func readUsageEstimateDefaults(ctx context.Context, q queryRower, task string) (api.UsageEstimateDefaults, error) {
	var out api.UsageEstimateDefaults
	err := q.QueryRowContext(ctx, `SELECT small_tokens,small_race_tokens,planned_tokens,planned_race_tokens,updated_at,by_agent,by_node,by_user FROM usage_estimate_defaults WHERE task_id=?`, task).
		Scan(&out.SmallTokens, &out.SmallRaceTokens, &out.PlannedTokens, &out.PlannedRaceTokens, &out.UpdatedAt, &out.UpdatedBy.AgentID, &out.UpdatedBy.Node, &out.UpdatedBy.User)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	out.Configured = err == nil
	return out, err
}

// UsageEstimateDefaults reads the project's lane default estimates.
func (s *Store) UsageEstimateDefaults(ctx context.Context, task string) (api.UsageEstimateDefaults, error) {
	if !api.ValidID(task, "tsk") {
		return api.UsageEstimateDefaults{}, api.ErrInvalid
	}
	if err := usageProjectExists(ctx, s.db, task); err != nil {
		return api.UsageEstimateDefaults{}, err
	}
	return readUsageEstimateDefaults(ctx, s.db, task)
}

// SetUsageEstimateDefaults saves the four lane defaults. They are data, not
// constants: the backlog steward recomputes them from history.
func (s *Store) SetUsageEstimateDefaults(ctx context.Context, task string, req api.UsageEstimateDefaultsRequest, by api.Caller) (api.UsageEstimateDefaults, error) {
	zero := api.UsageEstimateDefaults{}
	if !api.ValidID(task, "tsk") || (req.AgentID != "" && !api.ValidID(req.AgentID, "agt")) {
		return zero, api.ErrInvalid
	}
	for _, tokens := range []int64{req.SmallTokens, req.SmallRaceTokens, req.PlannedTokens, req.PlannedRaceTokens} {
		if tokens < 1 || tokens > api.MaxEstimateTokens {
			return zero, usageInvalid("each lane default must be 1 to " + strconv.FormatInt(api.MaxEstimateTokens, 10) + " tokens")
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if err = usageProjectExists(ctx, tx, task); err != nil {
		return zero, err
	}
	if err = usageSettingsWriter(ctx, tx, task, req.AgentID, "the lane default estimates"); err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_estimate_defaults(task_id,small_tokens,small_race_tokens,planned_tokens,planned_race_tokens,updated_at,by_agent,by_node,by_user) VALUES(?,?,?,?,?,?,?,?,?)
 ON CONFLICT(task_id) DO UPDATE SET small_tokens=excluded.small_tokens,small_race_tokens=excluded.small_race_tokens,planned_tokens=excluded.planned_tokens,planned_race_tokens=excluded.planned_race_tokens,updated_at=excluded.updated_at,by_agent=excluded.by_agent,by_node=excluded.by_node,by_user=excluded.by_user`,
		task, req.SmallTokens, req.SmallRaceTokens, req.PlannedTokens, req.PlannedRaceTokens, ts(s.now()), req.AgentID, by.Node, by.User); err != nil {
		return zero, err
	}
	out, err := readUsageEstimateDefaults(ctx, tx, task)
	if err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

func readProviderUsage(ctx context.Context, q queryRower, host string) (api.ProviderUsage, error) {
	out := api.ProviderUsage{Host: host, Readings: []api.ProviderReading{}}
	rows, err := q.QueryContext(ctx, `SELECT runtime,window,state,used_percent,resets_at,captured_at,reported_at,version FROM usage_provider_readings WHERE host=? ORDER BY runtime,window`, host)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var r api.ProviderReading
		if err := rows.Scan(&r.Runtime, &r.Window, &r.State, &r.UsedPercent, &r.ResetsAt, &r.CapturedAt, &r.ReportedAt, &r.Version); err != nil {
			return out, err
		}
		out.Readings = append(out.Readings, r)
	}
	return out, rows.Err()
}

// ProviderUsage reads every stored provider reading of one host.
func (s *Store) ProviderUsage(ctx context.Context, host string) (api.ProviderUsage, error) {
	if host == "" || len(host) > 255 {
		return api.ProviderUsage{}, api.ErrInvalid
	}
	return readProviderUsage(ctx, s.db, host)
}

// ReportProviderUsage replaces a host's reading for one runtime with what its
// relay just read from the runtime's usage capture. A state other than ok is
// an invalidation: both windows are overwritten with the state and no figure,
// so nothing of an earlier reading remains to be mistaken for headroom.
func (s *Store) ReportProviderUsage(ctx context.Context, report api.ProviderUsageReport) (api.ProviderUsage, error) {
	zero := api.ProviderUsage{}
	if report.Host == "" || len(report.Host) > 255 || strings.ContainsRune(report.Host, '\x00') || !usageBudgetRuntime(report.Runtime) || len(report.Version) > 80 {
		return zero, api.ErrInvalid
	}
	type reading struct{ window, used, resets string }
	var readings []reading
	captured := ""
	switch report.State {
	case api.ProviderUsageOK:
		at, err := time.Parse(time.RFC3339Nano, report.CapturedAt)
		if err != nil || len(report.Windows) == 0 {
			return zero, usageInvalid("an ok provider reading needs its capture time and at least one window")
		}
		captured = ts(at)
		seen := map[string]bool{}
		for _, w := range report.Windows {
			resets, err := time.Parse(time.RFC3339Nano, w.ResetsAt)
			if api.UsageWindowLength(w.Window) == 0 || seen[w.Window] || err != nil || !(w.UsedPercent >= 0 && w.UsedPercent <= 100) {
				return zero, usageInvalid("a provider window needs a known name, a used percentage from 0 to 100 and a reset time")
			}
			seen[w.Window] = true
			readings = append(readings, reading{w.Window, strconv.FormatFloat(w.UsedPercent, 'f', -1, 64), ts(resets)})
		}
	case api.ProviderUsageMissing, api.ProviderUsageUnreadable, api.ProviderUsageMalformed:
		if len(report.Windows) != 0 {
			return zero, usageInvalid("an invalidation carries no figures")
		}
		for _, window := range usageBudgetWindows {
			readings = append(readings, reading{window: window})
		}
	default:
		return zero, usageInvalid("provider reading state must be ok, missing, unreadable or malformed")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_provider_readings WHERE host=? AND runtime=?`, report.Host, report.Runtime); err != nil {
		return zero, err
	}
	version := report.Version
	if report.State != api.ProviderUsageOK {
		version = ""
	}
	now := ts(s.now())
	for _, r := range readings {
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_provider_readings(host,runtime,window,state,used_percent,resets_at,captured_at,reported_at,version) VALUES(?,?,?,?,?,?,?,?,?)`,
			report.Host, report.Runtime, r.window, report.State, r.used, r.resets, captured, now, version); err != nil {
			return zero, err
		}
	}
	out, err := readProviderUsage(ctx, tx, report.Host)
	if err != nil {
		return zero, err
	}
	return out, tx.Commit()
}
