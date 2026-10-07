package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler arms (docs/handler-ab.md). An enabled project policy lists arms of
// database handlers (runtime, model, reasoning). The team queue claim draws
// an arm reproducibly from the policy seed and entry ID and leases a free
// handler of it. Provider-limit episodes take an arm out of the draw.
func migrateHandlerAB(db *sql.DB) error {
	for _, column := range []string{"model", "reasoning"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('handler_runs') WHERE name=?`, column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE handler_runs ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS handler_arm_policy (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id), enabled INTEGER NOT NULL, seed TEXT NOT NULL, fallback INTEGER NOT NULL,
 limit_hold_minutes INTEGER NOT NULL, template_digest TEXT NOT NULL, arms_json TEXT NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS handler_arm_policy_requests (
 task_id TEXT NOT NULL, request_id TEXT NOT NULL, payload_hash TEXT NOT NULL, result_json TEXT NOT NULL, PRIMARY KEY(task_id,request_id));
CREATE TABLE IF NOT EXISTS handler_arm_assignments (
 task_id TEXT NOT NULL, entry_id TEXT NOT NULL, item_id TEXT NOT NULL, lease_generation INTEGER NOT NULL, policy_revision INTEGER NOT NULL,
 draw_hex TEXT NOT NULL, drawn_arm TEXT NOT NULL, arm TEXT NOT NULL, fallback INTEGER NOT NULL, fallback_reason TEXT NOT NULL DEFAULT '',
 skipped_json TEXT NOT NULL DEFAULT '[]', handler_id TEXT NOT NULL, handler_run_id TEXT NOT NULL, handler_digest TEXT NOT NULL DEFAULT '',
 policy_digest TEXT NOT NULL DEFAULT '', leased_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(entry_id,handler_id,lease_generation));
CREATE INDEX IF NOT EXISTS handler_arm_assignments_task ON handler_arm_assignments(task_id,arm);
CREATE TABLE IF NOT EXISTS handler_arm_limit_episodes (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, arm TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, source TEXT NOT NULL,
 started_at TEXT NOT NULL, last_signal_at TEXT NOT NULL, hold_until TEXT NOT NULL DEFAULT '', cleared_at TEXT NOT NULL DEFAULT '',
 clear_reason TEXT NOT NULL DEFAULT '', start_notice_seq INTEGER NOT NULL DEFAULT 0, clear_notice_seq INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS handler_arm_limit_open ON handler_arm_limit_episodes(task_id,arm) WHERE cleared_at='';
CREATE TABLE IF NOT EXISTS handler_write_refusals (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, item_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, route TEXT NOT NULL,
 status INTEGER NOT NULL, code TEXT NOT NULL CHECK(code IN ('invalid','not_found','conflict','stale_revision')), request_id TEXT NOT NULL DEFAULT '', at TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS handler_write_refusals_request ON handler_write_refusals(agent_id,run_id,request_id) WHERE request_id<>'';
CREATE INDEX IF NOT EXISTS handler_write_refusals_item ON handler_write_refusals(task_id,item_id);
CREATE TRIGGER IF NOT EXISTS handler_write_refusals_no_update BEFORE UPDATE ON handler_write_refusals
BEGIN SELECT RAISE(ABORT,'handler write refusals are immutable'); END;
CREATE TRIGGER IF NOT EXISTS handler_write_refusals_no_delete BEFORE DELETE ON handler_write_refusals
BEGIN SELECT RAISE(ABORT,'handler write refusals are immutable'); END;
CREATE TABLE IF NOT EXISTS handler_provisions (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, entry_id TEXT NOT NULL, arm TEXT NOT NULL DEFAULT '', agent_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL CHECK(state IN ('reserved','registered','abandoned','refused')), reason TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '',
 host TEXT NOT NULL DEFAULT '', spec_runtime TEXT NOT NULL DEFAULT '', spec_model TEXT NOT NULL DEFAULT '', spec_reasoning TEXT NOT NULL DEFAULT '',
 spec_digest TEXT NOT NULL DEFAULT '', fix TEXT NOT NULL DEFAULT '', notice_seq INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS handler_provisions_entry ON handler_provisions(task_id,entry_id);
CREATE UNIQUE INDEX IF NOT EXISTS handler_provisions_pending ON handler_provisions(task_id) WHERE state='reserved';
CREATE UNIQUE INDEX IF NOT EXISTS handler_provisions_refused ON handler_provisions(entry_id) WHERE state='refused';`)
	return err
}

var handlerArmIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)
var handlerArmFieldPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$`)

// validArmField accepts an empty value or a model, runtime or reasoning name
// in the shape tt spawn accepts.
func validArmField(v string) bool { return v == "" || handlerArmFieldPattern.MatchString(v) }

func defaultHandlerArmPolicy(task string) api.HandlerArmPolicy {
	return api.HandlerArmPolicy{TaskID: task, LimitHoldMinutes: api.DefaultHandlerArmLimitHoldMinutes, Arms: []api.HandlerArm{}}
}

func loadHandlerArmPolicy(ctx context.Context, q queryRower, task string) (api.HandlerArmPolicy, error) {
	p := defaultHandlerArmPolicy(task)
	var arms, updated string
	err := q.QueryRowContext(ctx, `SELECT enabled,seed,fallback,limit_hold_minutes,template_digest,arms_json,revision,updated_at FROM handler_arm_policy WHERE task_id=?`, task).
		Scan(&p.Enabled, &p.Seed, &p.Fallback, &p.LimitHoldMinutes, &p.TemplateDigest, &arms, &p.Revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(arms), &p.Arms); err != nil {
		return p, fmt.Errorf("handler arm policy arms: %w", err)
	}
	t := parseTS(updated)
	p.UpdatedAt = &t
	return p, nil
}

func invalidArmPolicy(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{api.ErrInvalid}, args...)...)
}

// validateHandlerArmPolicy applies the policy rules; each failure is a 400.
func validateHandlerArmPolicy(req api.HandlerArmPolicyRequest) error {
	if req.ExpectedRevision < 0 {
		return invalidArmPolicy("expected revision must not be negative")
	}
	if len(req.Arms) < 2 || len(req.Arms) > 8 {
		return invalidArmPolicy("a policy has 2 to 8 arms")
	}
	if req.Fallback && len(req.Arms) < 2 {
		return invalidArmPolicy("fallback needs at least 2 arms")
	}
	if req.LimitHoldMinutes != 0 && (req.LimitHoldMinutes < 5 || req.LimitHoldMinutes > 1440) {
		return invalidArmPolicy("limit hold must be 5 to 1440 minutes")
	}
	if strings.TrimSpace(req.Seed) == "" || !api.ValidText(req.Seed, 128) || strings.ContainsAny(req.Seed, "\r\n") {
		return invalidArmPolicy("the seed must be non-empty text of at most 128 characters")
	}
	// Only an enabled policy needs the reference template; disabling one
	// never depends on a digest or a saved spec.
	if (req.Enabled || req.TemplateDigest != "") && (len(req.TemplateDigest) != 64 || strings.Trim(req.TemplateDigest, "0123456789abcdef") != "") {
		return invalidArmPolicy("an enabled policy needs a template digest of 64 lowercase hex characters")
	}
	ids := map[string]bool{}
	triples := map[string]bool{}
	for _, arm := range req.Arms {
		if !handlerArmIDPattern.MatchString(arm.ID) {
			return invalidArmPolicy("arm ID %q must match [A-Za-z0-9_-]{1,16}", arm.ID)
		}
		if ids[arm.ID] {
			return invalidArmPolicy("arm ID %s is repeated", arm.ID)
		}
		ids[arm.ID] = true
		if arm.Weight < 1 || arm.Weight > 1000 {
			return invalidArmPolicy("arm %s weight must be an integer from 1 to 1000", arm.ID)
		}
		if arm.Runtime == "" || arm.Model == "" || arm.Reasoning == "" || !validArmField(arm.Runtime) || !validArmField(arm.Model) || !validArmField(arm.Reasoning) {
			return invalidArmPolicy("arm %s needs a runtime, model and reasoning", arm.ID)
		}
		key := arm.Runtime + "\x00" + arm.Model + "\x00" + arm.Reasoning
		if triples[key] {
			return invalidArmPolicy("arm %s repeats another arm's runtime, model and reasoning", arm.ID)
		}
		triples[key] = true
	}
	return nil
}

func sortedArms(arms []api.HandlerArm) []api.HandlerArm {
	out := append([]api.HandlerArm(nil), arms...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// handlerArmFor names the arm a run matches: its agent runtime and the model
// and reasoning recorded at spawn must all be equal. The template digest is
// deliberately not part of the identity (lead amendment #15761): a rotation
// after a template change stays leasable and the report flags its items.
func handlerArmFor(p api.HandlerArmPolicy, runtime, model, reasoning string) string {
	if model == "" {
		return ""
	}
	for _, arm := range p.Arms {
		if arm.Runtime == runtime && arm.Model == model && arm.Reasoning == reasoning {
			return arm.ID
		}
	}
	return ""
}

type handlerRunRecord struct {
	digest, model, reasoning string
}

func loadHandlerRunRecord(ctx context.Context, q queryRower, agentID, runID string) (handlerRunRecord, error) {
	var r handlerRunRecord
	err := q.QueryRowContext(ctx, `SELECT template_digest,model,reasoning FROM handler_runs WHERE agent_id=? AND run_id=?`, agentID, runID).Scan(&r.digest, &r.model, &r.reasoning)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	return r, err
}

// armHandlers lists the project's open database handlers with their recorded
// run fields and arm.
func armHandlers(ctx context.Context, q queryRower, p api.HandlerArmPolicy) ([]api.HandlerArmHandler, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?) ORDER BY created_at,id`, p.TaskID, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited)
	if err != nil {
		return nil, err
	}
	var agents []api.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		agents = append(agents, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []api.HandlerArmHandler{}
	for _, a := range agents {
		rec, err := loadHandlerRunRecord(ctx, q, a.ID, a.RunID)
		if err != nil {
			return nil, err
		}
		h := api.HandlerArmHandler{AgentID: a.ID, Name: a.Name, RunID: a.RunID, Status: a.Status, Online: a.Online, Runtime: a.Runtime,
			Model: rec.model, Reasoning: rec.reasoning, TemplateDigest: rec.digest, DigestMatches: rec.digest != "" && rec.digest == p.TemplateDigest}
		h.Arm = handlerArmFor(p, a.Runtime, rec.model, rec.reasoning)
		if h.Arm == "" {
			if rec.model == "" {
				h.NoArmReason = "no model recorded at spawn"
			} else {
				h.NoArmReason = "runtime, model and reasoning match no arm"
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// HandlerArmPolicy returns the policy with every open handler's arm and
// recorded digest, and the open limit episodes.
func (s *Store) HandlerArmPolicy(ctx context.Context, task string) (api.HandlerArmPolicyView, error) {
	var out api.HandlerArmPolicyView
	if _, err := s.GetTask(ctx, task); err != nil {
		return out, err
	}
	p, err := loadHandlerArmPolicy(ctx, s.db, task)
	if err != nil {
		return out, err
	}
	out.Policy = p
	if out.Handlers, err = armHandlers(ctx, s.db, p); err != nil {
		return out, err
	}
	out.Limits, err = loadArmLimits(ctx, s.db, task, true)
	return out, err
}

func (s *Store) SetHandlerArmPolicy(ctx context.Context, task string, req api.HandlerArmPolicyRequest) (api.HandlerArmPolicy, error) {
	var zero api.HandlerArmPolicy
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) {
		return zero, api.ErrInvalid
	}
	if req.ActorAgentID != "" {
		return zero, &api.HandlerArmRefusal{Code: api.HandlerArmRefusedAgentCaller, Detail: "the handler arm policy is the owner's; agent sessions cannot change it"}
	}
	if err := validateHandlerArmPolicy(req); err != nil {
		return zero, err
	}
	hash := requestHash(req)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var priorHash, prior string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,result_json FROM handler_arm_policy_requests WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&priorHash, &prior)
	if err == nil {
		if priorHash != hash {
			return zero, &api.HandlerArmRefusal{Code: api.HandlerArmRefusedRequestReused, Detail: "the request ID was already used with different input"}
		}
		var out api.HandlerArmPolicy
		return out, json.Unmarshal([]byte(prior), &out)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	if t.Status != api.TaskOpen {
		return zero, api.ErrClosed
	}
	current, err := loadHandlerArmPolicy(ctx, tx, task)
	if err != nil {
		return zero, err
	}
	if current.Revision != req.ExpectedRevision {
		return zero, &api.HandlerArmRefusal{Code: api.HandlerArmRefusedStalePolicy, Detail: fmt.Sprintf("policy is at revision %d; reload before changing it", current.Revision)}
	}
	now := s.now()
	out := api.HandlerArmPolicy{TaskID: task, Enabled: req.Enabled, Seed: req.Seed, Fallback: req.Fallback, LimitHoldMinutes: req.LimitHoldMinutes,
		TemplateDigest: req.TemplateDigest, Arms: sortedArms(req.Arms), Revision: current.Revision + 1, UpdatedAt: &now}
	if out.LimitHoldMinutes == 0 {
		out.LimitHoldMinutes = api.DefaultHandlerArmLimitHoldMinutes
	}
	if out.Enabled {
		// Template parity: every open handler that would be leased under this
		// policy must have started from the policy's template.
		handlers, err := armHandlers(ctx, tx, out)
		if err != nil {
			return zero, err
		}
		var mismatched []string
		for _, h := range handlers {
			if h.Arm != "" && !h.DigestMatches {
				digest := h.TemplateDigest
				if digest == "" {
					digest = "missing"
				} else {
					digest = digest[:12]
				}
				mismatched = append(mismatched, fmt.Sprintf("%s (%s/%s, arm %s, digest %s)", h.Name, h.AgentID, h.RunID, h.Arm, digest))
			}
		}
		if len(mismatched) > 0 {
			return zero, &api.HandlerArmRefusal{Code: api.HandlerArmRefusedTemplate, Detail: "these handlers did not start from the policy template: " + strings.Join(mismatched, ", ")}
		}
	}
	arms, _ := json.Marshal(out.Arms)
	if _, err = tx.ExecContext(ctx, `INSERT INTO handler_arm_policy(task_id,enabled,seed,fallback,limit_hold_minutes,template_digest,arms_json,revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?)
 ON CONFLICT(task_id) DO UPDATE SET enabled=excluded.enabled,seed=excluded.seed,fallback=excluded.fallback,limit_hold_minutes=excluded.limit_hold_minutes,
 template_digest=excluded.template_digest,arms_json=excluded.arms_json,revision=excluded.revision,updated_at=excluded.updated_at`,
		task, out.Enabled, out.Seed, out.Fallback, out.LimitHoldMinutes, out.TemplateDigest, string(arms), out.Revision, ts(now)); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, task, "task_updated", "", "Handler arm policy saved", map[string]any{"handlerArmPolicy": out}, api.Caller{Node: "workspace", User: "owner"}); err != nil {
		return zero, err
	}
	encoded, _ := json.Marshal(out)
	if _, err = tx.ExecContext(ctx, `INSERT INTO handler_arm_policy_requests(task_id,request_id,payload_hash,result_json) VALUES(?,?,?,?)`, task, req.RequestID, hash, string(encoded)); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

// handlerArmDraw picks an arm for an entry: u is the big-endian uint64 of the
// first 8 bytes of SHA-256(seed NUL entryID); with the arms sorted by ID, the
// first arm whose cumulative weight c satisfies u < floor(2^64·c/total) wins.
// It returns the arm and the 8-byte draw in hex. Arms must be non-empty.
func handlerArmDraw(seed, entryID string, arms []api.HandlerArm) (string, string) {
	h := sha256.Sum256([]byte(seed + "\x00" + entryID))
	u := new(big.Int).SetUint64(binary.BigEndian.Uint64(h[:8]))
	sorted := sortedArms(arms)
	total := int64(0)
	for _, arm := range sorted {
		total += int64(arm.Weight)
	}
	space := new(big.Int).Lsh(big.NewInt(1), 64)
	cumulative := int64(0)
	for _, arm := range sorted {
		cumulative += int64(arm.Weight)
		bound := new(big.Int).Mul(space, big.NewInt(cumulative))
		bound.Quo(bound, big.NewInt(total))
		if u.Cmp(bound) < 0 {
			return arm.ID, hex.EncodeToString(h[:8])
		}
	}
	return sorted[len(sorted)-1].ID, hex.EncodeToString(h[:8])
}

// armLeaseError is a 409 wait: the runner treats it as a benign retry.
func armLeaseError(message string) error {
	return fmt.Errorf("%w: %s", api.ErrConflict, message)
}

func armBusyMessage(arm string) string {
	return "arm " + arm + ": " + api.HandlerArmWaitSuffix
}

const armAllLimitedMessage = "every handler arm is at a provider limit, so there is " + api.HandlerArmWaitSuffix

// armDecision is what the draw and lease would do for one entry now.
type armDecision struct {
	policy   api.HandlerArmPolicy
	drawHex  string
	drawn    string
	arm      string
	fallback bool
	skipped  []string
	chosen   api.Agent
	// wait is the conflict message when no handler can be leased.
	wait string
}

// decideHandlerArm draws an arm for entryID over the arms that are not
// limited and picks a free handler of it, falling back to another arm only
// when the policy allows. It writes nothing.
func decideHandlerArm(ctx context.Context, q queryRower, p api.HandlerArmPolicy, entryID string, active []api.TeamQueueEntry, now time.Time) (armDecision, error) {
	d := armDecision{policy: p, skipped: []string{}}
	limited, err := limitedArms(ctx, q, p.TaskID, now)
	if err != nil {
		return d, err
	}
	var available []api.HandlerArm
	for _, arm := range sortedArms(p.Arms) {
		if limited[arm.ID] {
			d.skipped = append(d.skipped, arm.ID)
		} else {
			available = append(available, arm)
		}
	}
	if len(available) == 0 {
		d.wait = armAllLimitedMessage
		return d, nil
	}
	d.drawn, d.drawHex = handlerArmDraw(p.Seed, entryID, available)
	free, err := freeQueueHandlers(ctx, q, p.TaskID, active)
	if err != nil {
		return d, err
	}
	byArm := map[string]api.Agent{}
	for _, a := range free {
		rec, err := loadHandlerRunRecord(ctx, q, a.ID, a.RunID)
		if err != nil {
			return d, err
		}
		if arm := handlerArmFor(p, a.Runtime, rec.model, rec.reasoning); arm != "" {
			if _, seen := byArm[arm]; !seen {
				byArm[arm] = a
			}
		}
	}
	if a, ok := byArm[d.drawn]; ok {
		d.arm, d.chosen = d.drawn, a
		return d, nil
	}
	if p.Fallback {
		for _, arm := range available {
			if a, ok := byArm[arm.ID]; ok && arm.ID != d.drawn {
				d.arm, d.chosen, d.fallback = arm.ID, a, true
				return d, nil
			}
		}
	}
	d.wait = armBusyMessage(d.drawn)
	return d, nil
}

// leaseQueueHandler chooses the handler a claim leases. Without an enabled
// arm policy it is the first free handler, as before; a zero agent then means
// none is free. Under a policy it is the arm decision, or an arm-wait 409.
func leaseQueueHandler(ctx context.Context, tx *sql.Tx, task, entryID string, active []api.TeamQueueEntry, now time.Time) (api.Agent, *api.TeamQueueHandlerArm, error) {
	p, err := loadHandlerArmPolicy(ctx, tx, task)
	if err != nil {
		return api.Agent{}, nil, err
	}
	if !p.Enabled {
		a, err := freeQueueHandler(ctx, tx, task, active)
		return a, nil, err
	}
	d, err := decideHandlerArm(ctx, tx, p, entryID, active, now)
	if err != nil {
		return api.Agent{}, nil, err
	}
	if d.wait != "" {
		return api.Agent{}, nil, armLeaseError(d.wait)
	}
	rec, err := loadHandlerRunRecord(ctx, tx, d.chosen.ID, d.chosen.RunID)
	if err != nil {
		return api.Agent{}, nil, err
	}
	assignment := &api.TeamQueueHandlerArm{PolicyRevision: p.Revision, Draw: d.drawHex, DrawnArm: d.drawn, Arm: d.arm, Fallback: d.fallback,
		SkippedLimited: d.skipped, HandlerID: d.chosen.ID, HandlerRunID: d.chosen.RunID, HandlerDigest: rec.digest, PolicyDigest: p.TemplateDigest, LeasedAt: ts(now)}
	if d.fallback {
		assignment.FallbackReason = api.HandlerArmFallbackBusy
	}
	return d.chosen, assignment, nil
}

func insertHandlerArmAssignment(ctx context.Context, tx *sql.Tx, task string, e api.TeamQueueEntry, a api.TeamQueueHandlerArm) error {
	skipped, _ := json.Marshal(a.SkippedLimited)
	_, err := tx.ExecContext(ctx, `INSERT INTO handler_arm_assignments(task_id,entry_id,item_id,lease_generation,policy_revision,draw_hex,drawn_arm,arm,fallback,fallback_reason,skipped_json,handler_id,handler_run_id,handler_digest,policy_digest,leased_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, task, e.ID, e.ItemID, a.LeaseGeneration, a.PolicyRevision, a.Draw, a.DrawnArm, a.Arm, a.Fallback, a.FallbackReason,
		string(skipped), a.HandlerID, a.HandlerRunID, a.HandlerDigest, a.PolicyDigest, a.LeasedAt)
	return err
}

// finishHandlerArmAssignment stamps the current lease's assignment when its
// entry finishes.
func finishHandlerArmAssignment(ctx context.Context, tx *sql.Tx, e api.TeamQueueEntry, now string) error {
	_, err := tx.ExecContext(ctx, `UPDATE handler_arm_assignments SET finished_at=? WHERE entry_id=? AND handler_id=? AND lease_generation=? AND finished_at=''`, now, e.ID, e.HandlerID, e.HandlerLeaseGeneration)
	return err
}

const handlerArmAssignmentCols = `entry_id,item_id,lease_generation,policy_revision,draw_hex,drawn_arm,arm,fallback,fallback_reason,skipped_json,handler_id,handler_run_id,handler_digest,policy_digest,leased_at,finished_at`

type armAssignmentRow struct {
	entryID, itemID string
	api.TeamQueueHandlerArm
}

func scanHandlerArmAssignment(row interface{ Scan(...any) error }) (armAssignmentRow, error) {
	var r armAssignmentRow
	var skipped string
	err := row.Scan(&r.entryID, &r.itemID, &r.LeaseGeneration, &r.PolicyRevision, &r.Draw, &r.DrawnArm, &r.Arm, &r.Fallback, &r.FallbackReason, &skipped,
		&r.HandlerID, &r.HandlerRunID, &r.HandlerDigest, &r.PolicyDigest, &r.LeasedAt, &r.FinishedAt)
	if err != nil {
		return r, err
	}
	r.SkippedLimited = []string{}
	if err := json.Unmarshal([]byte(skipped), &r.SkippedLimited); err != nil {
		return r, fmt.Errorf("handler arm assignment skipped arms: %w", err)
	}
	return r, nil
}

// attachHandlerArm fills an entry's HandlerArm from its current lease.
func attachHandlerArm(ctx context.Context, q queryRower, e *api.TeamQueueEntry) error {
	if e.HandlerID == "" || e.HandlerLeaseGeneration < 1 {
		return nil
	}
	r, err := scanHandlerArmAssignment(q.QueryRowContext(ctx, `SELECT `+handlerArmAssignmentCols+` FROM handler_arm_assignments WHERE entry_id=? AND handler_id=? AND lease_generation=?`, e.ID, e.HandlerID, e.HandlerLeaseGeneration))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	arm := r.TeamQueueHandlerArm
	e.HandlerArm = &arm
	return nil
}

// explainArmWaits replaces the generic free-handler reason on queued entries
// with the arm the entry waits for, reading the same episode predicate as
// the draw. An entry that waits only for a handler also gets its HandlerNeed
// and, when one could be added, a reason with the counts and the fix.
func explainArmWaits(ctx context.Context, q queryRower, list *api.TeamQueueList, active []api.TeamQueueEntry, maxAgents int, now time.Time) error {
	if len(list.Entries) == 0 {
		return nil
	}
	task := list.Entries[0].TaskID
	p, err := loadHandlerArmPolicy(ctx, q, task)
	if err != nil {
		return err
	}
	var nc *handlerNeedContext
	for i := range list.Entries {
		e := &list.Entries[i]
		if e.State != "queued" || len(e.BlockedBy) > 0 || (e.BlockReason != "" && e.BlockReason != noFreeHandlerReason) {
			continue
		}
		drawn := ""
		if p.Enabled {
			d, err := decideHandlerArm(ctx, q, p, e.ID, active, now)
			if err != nil {
				return err
			}
			switch {
			case d.wait == armAllLimitedMessage:
				e.BlockReason = "Every handler arm is at a provider limit"
				continue
			case d.wait != "":
				e.BlockReason = "Waiting for a free handler of arm " + d.drawn
				drawn = d.drawn
			default:
				if e.BlockReason == noFreeHandlerReason {
					e.BlockReason = ""
				}
				continue
			}
		} else if e.BlockReason != noFreeHandlerReason {
			continue
		}
		if nc == nil {
			if nc, err = loadHandlerNeedContext(ctx, q, p, active, list.ConcurrencyLimit, maxAgents, now); err != nil {
				return err
			}
		}
		need, tail, err := nc.need(ctx, q, *e, drawn, false)
		if err != nil {
			return err
		}
		if need == nil {
			continue
		}
		e.HandlerNeed = need
		e.BlockReason += tail
	}
	return nil
}

// Provider-limit episodes.

const armLimitCols = `id,arm,agent_id,run_id,source,started_at,last_signal_at,hold_until,cleared_at,clear_reason,start_notice_seq,clear_notice_seq`

func scanArmLimit(row interface{ Scan(...any) error }) (api.HandlerArmLimit, error) {
	var l api.HandlerArmLimit
	var started, last, hold, cleared string
	err := row.Scan(&l.ID, &l.Arm, &l.AgentID, &l.RunID, &l.Source, &started, &last, &hold, &cleared, &l.ClearReason, &l.StartNoticeSeq, &l.ClearNoticeSeq)
	if err != nil {
		return l, err
	}
	l.StartedAt, l.LastSignalAt = parseTS(started), parseTS(last)
	if hold != "" {
		t := parseTS(hold)
		l.HoldUntil = &t
	}
	if cleared != "" {
		t := parseTS(cleared)
		l.ClearedAt = &t
	}
	return l, nil
}

func loadArmLimits(ctx context.Context, q queryRower, task string, openOnly bool) ([]api.HandlerArmLimit, error) {
	query := `SELECT ` + armLimitCols + ` FROM handler_arm_limit_episodes WHERE task_id=?`
	if openOnly {
		query += ` AND cleared_at=''`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY started_at,id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.HandlerArmLimit{}
	for rows.Next() {
		l, err := scanArmLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// armActivity is a run's latest stored activity.
type armActivity struct {
	state, reason, promptKind, promptOutcome, providerClass string
	observedAt                                              time.Time
}

func loadArmActivity(ctx context.Context, q queryRower, agentID, runID string) (*armActivity, bool, error) {
	var status, currentRun string
	err := q.QueryRowContext(ctx, `SELECT status,run_id FROM agents WHERE id=?`, agentID).Scan(&status, &currentRun)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	runOpen := err == nil && currentRun == runID && status != api.AgentClosed && status != api.AgentExited
	var payload string
	err = q.QueryRowContext(ctx, `SELECT payload FROM agent_activity WHERE agent_id=? AND run_id=?`, agentID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runOpen, nil
	}
	if err != nil {
		return nil, runOpen, err
	}
	var a api.AgentActivity
	if err := json.Unmarshal([]byte(payload), &a); err != nil {
		return nil, runOpen, fmt.Errorf("activity snapshot: %w", err)
	}
	return activityForArm(a), runOpen, nil
}

func activityForArm(a api.AgentActivity) *armActivity {
	out := &armActivity{state: a.State, reason: a.Reason, observedAt: a.ObservedAt.UTC()}
	if a.Prompt != nil {
		out.promptKind, out.promptOutcome = a.Prompt.Kind, a.Prompt.Outcome
	}
	if a.Provider != nil {
		out.providerClass = a.Provider.Class
	}
	return out
}

// armLimitSignal names the limit a report shows for a handler of runtime,
// or "" when it shows none.
func armLimitSignal(runtime string, a *armActivity) string {
	if a == nil {
		return ""
	}
	switch runtime {
	case "codex":
		// A prompt an action answered is gone.
		if a.state == "runtime_prompt" && a.promptKind == api.RuntimePromptCodexUsageLimit && a.promptOutcome != api.RuntimePromptConfirmed {
			return api.HandlerArmSourceCodexUsageLimit
		}
	case "claude":
		// The provider_blocked form (docs/provider-blocked.md), or the turn-end
		// reason an older relay sends and a single rate-limited turn still has.
		if a.state == "provider_blocked" && (a.providerClass == api.ProviderBlockUsageLimit || a.providerClass == api.ProviderBlockRateLimited) {
			return api.HandlerArmSourceClaudeRateLimit
		}
		if (a.state == "idle" || a.state == "finished_silent") && a.reason == api.ClaudeRateLimitReason {
			return api.HandlerArmSourceClaudeRateLimit
		}
	}
	return ""
}

// episodeClears is the one clearing rule (docs/handler-ab.md):
//   - Codex: the run's latest report no longer shows the usage-limit prompt,
//     or the run is closed.
//   - Claude: the hold has expired and either a completed turn without the
//     rate-limit reason was observed after the last signal, or no report has
//     arrived since it. A working report keeps the episode open. A closed run
//     clears once the hold expires.
func episodeClears(ep api.HandlerArmLimit, latest *armActivity, runOpen bool, now time.Time) (bool, string) {
	if ep.Source == api.HandlerArmSourceCodexUsageLimit {
		if !runOpen {
			return true, api.HandlerArmClearRunClosed
		}
		if latest != nil && armLimitSignal("codex", latest) == "" {
			return true, api.HandlerArmClearCodexPromptGone
		}
		return false, ""
	}
	if ep.HoldUntil != nil && now.Before(*ep.HoldUntil) {
		return false, ""
	}
	if !runOpen {
		return true, api.HandlerArmClearRunClosed
	}
	if latest == nil || !latest.observedAt.After(ep.LastSignalAt) {
		return true, api.HandlerArmClearHoldExpiredNoReport
	}
	if (latest.state == "idle" || latest.state == "finished_silent") && armLimitSignal("claude", latest) == "" {
		return true, api.HandlerArmClearHoldExpiredClean
	}
	return false, ""
}

// limitedArms is the set of arms with an open episode that does not clear
// now. The draw, the list reason and the report all read it.
func limitedArms(ctx context.Context, q queryRower, task string, now time.Time) (map[string]bool, error) {
	open, err := loadArmLimits(ctx, q, task, true)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, ep := range open {
		latest, runOpen, err := loadArmActivity(ctx, q, ep.AgentID, ep.RunID)
		if err != nil {
			return nil, err
		}
		if clears, _ := episodeClears(ep, latest, runOpen, now); !clears {
			out[ep.Arm] = true
		}
	}
	return out, nil
}

func armNotice(ctx context.Context, s *Store, tx *sql.Tx, t api.Task, subject, text, arm string) (int64, error) {
	env := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: map[string]string{"arm": arm}, Body: api.EnvelopeBody{Text: text}}
	m, err := s.insertMessage(ctx, tx, t, api.PostMessageRequest{Envelope: &env}, api.Agent{}, api.Caller{Node: "handler_arms", User: "hub"}, false, false)
	return m.Seq, err
}

func (s *Store) clearArmLimit(ctx context.Context, tx *sql.Tx, t api.Task, ep api.HandlerArmLimit, reason string, now time.Time) error {
	seq, err := armNotice(ctx, s, tx, t, "A handler arm is available again", "Handler arm "+ep.Arm+" is available again.", ep.Arm)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE handler_arm_limit_episodes SET cleared_at=?,clear_reason=?,clear_notice_seq=? WHERE id=? AND cleared_at=''`, ts(now), reason, seq, ep.ID)
	return err
}

// sweepArmLimits closes every open episode that clears now, posting one
// notice each. The claim runs it in its own transaction first, so a claim
// that then waits does not roll the clearing back.
func (s *Store) sweepArmLimits(ctx context.Context, task string) error {
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM handler_arm_policy WHERE task_id=?`, task).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !enabled) {
		return nil
	}
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	open, err := loadArmLimits(ctx, tx, task, true)
	if err != nil || len(open) == 0 {
		return err
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if err != nil {
		return err
	}
	now := s.now()
	changed := false
	for _, ep := range open {
		latest, runOpen, err := loadArmActivity(ctx, tx, ep.AgentID, ep.RunID)
		if err != nil {
			return err
		}
		if clears, reason := episodeClears(ep, latest, runOpen, now); clears {
			if err := s.clearArmLimit(ctx, tx, t, ep, reason, now); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify(task)
	return nil
}

// armLimitHook runs in the activity report transaction. A report of a
// handler run that matches an arm opens that arm's episode on a limit
// signal, or extends it; a report without one may clear it.
func (s *Store) armLimitHook(ctx context.Context, tx *sql.Tx, task, agent, run string, activity api.AgentActivity) error {
	p, err := loadHandlerArmPolicy(ctx, tx, task)
	if err != nil || !p.Enabled {
		return err
	}
	var role, runtime string
	if err := tx.QueryRowContext(ctx, `SELECT role,runtime FROM agents WHERE id=?`, agent).Scan(&role, &runtime); err != nil {
		return err
	}
	if role != api.AgentRoleDatabaseHandler {
		return nil
	}
	rec, err := loadHandlerRunRecord(ctx, tx, agent, run)
	if err != nil {
		return err
	}
	arm := handlerArmFor(p, runtime, rec.model, rec.reasoning)
	if arm == "" {
		return nil
	}
	ep, err := scanArmLimit(tx.QueryRowContext(ctx, `SELECT `+armLimitCols+` FROM handler_arm_limit_episodes WHERE task_id=? AND arm=? AND cleared_at=''`, task, arm))
	open := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := s.now()
	current := activityForArm(activity)
	signal := armLimitSignal(runtime, current)
	hold := ""
	if signal == api.HandlerArmSourceClaudeRateLimit {
		hold = ts(now.Add(time.Duration(p.LimitHoldMinutes) * time.Minute))
	}
	observed := ts(current.observedAt)
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if err != nil {
		return err
	}
	switch {
	case signal != "" && !open:
		seq, err := armNotice(ctx, s, tx, t, "A handler arm is at a provider limit", "Handler arm "+arm+" is at a provider limit; new items go to other arms.", arm)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO handler_arm_limit_episodes(id,task_id,arm,agent_id,run_id,source,started_at,last_signal_at,hold_until,start_notice_seq) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			api.NewID("hlim"), task, arm, agent, run, signal, observed, observed, hold, seq)
		return err
	case signal != "":
		// A repeat signal extends the hold and follows the latest run.
		_, err = tx.ExecContext(ctx, `UPDATE handler_arm_limit_episodes SET last_signal_at=?,hold_until=?,agent_id=?,run_id=?,source=? WHERE id=?`, observed, hold, agent, run, signal, ep.ID)
		return err
	case open && ep.AgentID == agent && ep.RunID == run:
		if clears, reason := episodeClears(ep, current, true, now); clears {
			return s.clearArmLimit(ctx, tx, t, ep, reason, now)
		}
	}
	return nil
}

// Handler write refusals (save quality).

// handlerWriteRefusalCode maps a refused write's HTTP status to its fixed
// code, or "" when that status is not recorded (403, 429 and the rest).
func handlerWriteRefusalCode(status int, err error) string {
	switch status {
	case 400:
		return "invalid"
	case 404:
		return "not_found"
	case 409:
		if err != nil && strings.Contains(err.Error(), "revision") {
			return "stale_revision"
		}
		return "conflict"
	}
	return ""
}

// RecordHandlerWriteRefusal keeps one refused work-item write by an open
// database handler of the project, in its own transaction after the refused
// one. Other callers are not recorded; a replayed request ID records once.
func (s *Store) RecordHandlerWriteRefusal(ctx context.Context, task, item, agentID, runID, route, requestID string, status int, refused error) error {
	code := handlerWriteRefusalCode(status, refused)
	if code == "" || agentID == "" || !api.ValidID(task, "tsk") || !api.ValidID(agentID, "agt") || len(route) > 64 || len(item) > 64 || len(requestID) > 128 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var role, status0, current string
	err := s.db.QueryRowContext(ctx, `SELECT role,status,run_id FROM agents WHERE id=? AND task_id=?`, agentID, task).Scan(&role, &status0, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if role != api.AgentRoleDatabaseHandler || status0 == api.AgentClosed || status0 == api.AgentExited {
		return nil
	}
	if runID == "" {
		runID = current
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO handler_write_refusals(id,task_id,item_id,agent_id,run_id,route,status,code,request_id,at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		api.NewID("hwr"), task, item, agentID, runID, route, status, code, requestID, ts(s.now()))
	return err
}

// HandlerWriteRefusals lists a project's recorded handler write refusals.
func (s *Store) HandlerWriteRefusals(ctx context.Context, task string) ([]api.HandlerWriteRefusal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,item_id,agent_id,run_id,route,status,code,request_id,at FROM handler_write_refusals WHERE task_id=? ORDER BY at,id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.HandlerWriteRefusal{}
	for rows.Next() {
		var r api.HandlerWriteRefusal
		var at string
		if err := rows.Scan(&r.ID, &r.ItemID, &r.AgentID, &r.RunID, &r.Route, &r.Status, &r.Code, &r.RequestID, &at); err != nil {
			return nil, err
		}
		r.At = parseTS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// handlerRotationArmRefusal keeps rotation inside an arm: when the project
// has a saved arm policy and the old run belongs to one of its arms, the
// successor must belong to the same arm. Without a policy, or for a run of
// no arm, rotation is unchanged. An owner-authorized change skips this check
// (commitHandlerRotation) and never writes the arm policy.
func handlerRotationArmRefusal(ctx context.Context, tx *sql.Tx, old, successor api.Agent) (*api.HandlerRotationRefusal, error) {
	p, err := loadHandlerArmPolicy(ctx, tx, old.TaskID)
	if err != nil || p.Revision == 0 {
		return nil, err
	}
	oldRec, err := loadHandlerRunRecord(ctx, tx, old.ID, old.RunID)
	if err != nil {
		return nil, err
	}
	arm := handlerArmFor(p, old.Runtime, oldRec.model, oldRec.reasoning)
	if arm == "" {
		return nil, nil
	}
	next, err := loadHandlerRunRecord(ctx, tx, successor.ID, successor.RunID)
	if err != nil {
		return nil, err
	}
	if handlerArmFor(p, successor.Runtime, next.model, next.reasoning) != arm {
		return &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedArmChanged, Detail: fmt.Sprintf("the successor runs %s/%s/%s but the old handler is in arm %s (%s/%s/%s); rotation stays within the handler arm",
			successor.Runtime, or(next.model, "-"), or(next.reasoning, "-"), arm, old.Runtime, oldRec.model, oldRec.reasoning)}, nil
	}
	return nil, nil
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Report (docs/handler-ab.md, metrics).

// abMedian is the middle value; with an even count, the mean of the two
// middle values floored to the unit. Nil when there are no values.
func abMedian(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	v := append([]int64(nil), values...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	n := len(v)
	m := v[n/2]
	if n%2 == 0 {
		sum := v[n/2-1] + v[n/2]
		m = sum / 2
		if sum < 0 && sum%2 != 0 {
			m--
		}
	}
	return &m
}

// abP90 is the nearest-rank 90th percentile: the ceil(0.9n)-th value.
func abP90(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	v := append([]int64(nil), values...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	rank := (9*len(v) + 9) / 10
	out := v[rank-1]
	return &out
}

// abMAD is the median absolute deviation from the median.
func abMAD(values []int64) *int64 {
	m := abMedian(values)
	if m == nil {
		return nil
	}
	dev := make([]int64, len(values))
	for i, v := range values {
		dev[i] = v - *m
		if dev[i] < 0 {
			dev[i] = -dev[i]
		}
	}
	return abMedian(dev)
}

// abNoiseFlag compares two arms' samples: insufficient below the minimum
// item count, within_noise when the medians differ by less than the larger
// MAD, otherwise difference.
func abNoiseFlag(nA, nB int, a, b []int64) string {
	if nA < api.HandlerArmNoiseMinItems || nB < api.HandlerArmNoiseMinItems {
		return "insufficient"
	}
	ma, mb := abMedian(a), abMedian(b)
	if ma == nil || mb == nil {
		return "insufficient"
	}
	diff := *ma - *mb
	if diff < 0 {
		diff = -diff
	}
	spread := *abMAD(a)
	if other := *abMAD(b); other > spread {
		spread = other
	}
	if diff < spread {
		return "within_noise"
	}
	return "difference"
}

// abCountFlag compares two arms' count totals: within_noise when they differ
// by 2 or less.
func abCountFlag(nA, nB, a, b int) string {
	if nA < api.HandlerArmNoiseMinItems || nB < api.HandlerArmNoiseMinItems {
		return "insufficient"
	}
	if d := a - b; d <= 2 && d >= -2 {
		return "within_noise"
	}
	return "difference"
}

func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	return r.RatString()
}

func parseRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// handlerChain is the leased handler and its committed rotation successors.
func handlerChain(ctx context.Context, q queryRower, agentID string) ([]string, error) {
	chain := []string{agentID}
	current := agentID
	for range 64 {
		var next string
		err := q.QueryRowContext(ctx, `SELECT successor_agent_id FROM handler_rotations WHERE old_agent_id=? AND state='committed'`, current).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, err
		}
		chain = append(chain, next)
		current = next
	}
	return chain, nil
}

func inList(column string, n int) string {
	return column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

func chainArgs(prefix []any, chain []string, suffix ...any) []any {
	out := append([]any(nil), prefix...)
	for _, id := range chain {
		out = append(out, id)
	}
	return append(out, suffix...)
}

func countQuery(ctx context.Context, q queryRower, query string, args ...any) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

var abCountKeys = []string{"handlerBlocks", "handlerAuthoredBlocks", "refusedSaves", "incorrectSaves", "ownerCorrections", "gateFixes", "linkCorrections", "interventions", "limitEvents"}

func abItemCounts(it api.HandlerABItem) map[string]int {
	interventions := 0
	for _, n := range it.Interventions {
		interventions += n
	}
	return map[string]int{"handlerBlocks": it.HandlerBlocks, "handlerAuthoredBlocks": it.HandlerAuthoredBlocks, "refusedSaves": it.RefusedSaves,
		"incorrectSaves": it.IncorrectSaves, "ownerCorrections": it.OwnerCorrections, "gateFixes": it.GateFixes, "linkCorrections": it.LinkCorrections,
		"interventions": interventions, "limitEvents": it.LimitEvents.Total}
}

// HandlerABReport compares the arms over finished entries that have an arm
// assignment for their final lease. Each item counts toward the arm that
// handled it.
func (s *Store) HandlerABReport(ctx context.Context, task string) (api.HandlerABReport, error) {
	out := api.HandlerABReport{TaskID: task, GeneratedAt: s.now(), Arms: []api.HandlerABArm{}, Items: []api.HandlerABItem{}, Comparisons: []api.HandlerABComparison{}}
	if _, err := s.GetTask(ctx, task); err != nil {
		return out, err
	}
	var err error
	if out.Policy, err = loadHandlerArmPolicy(ctx, s.db, task); err != nil {
		return out, err
	}
	if out.Limits, err = loadArmLimits(ctx, s.db, task, false); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixCols("a.", handlerArmAssignmentCols)+` FROM handler_arm_assignments a
 JOIN team_queue_entries e ON e.id=a.entry_id AND e.handler_id=a.handler_id AND e.handler_lease_generation=a.lease_generation
 WHERE a.task_id=? AND e.state='finished' ORDER BY e.position,a.entry_id`, task)
	if err != nil {
		return out, err
	}
	var assignments []armAssignmentRow
	for rows.Next() {
		r, err := scanHandlerArmAssignment(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		assignments = append(assignments, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	var usage api.UsageReport
	if len(assignments) > 0 {
		if usage, err = s.Usage(ctx, task, api.UsageQuery{}); err != nil {
			return out, err
		}
	}
	for _, a := range assignments {
		item, err := s.handlerABItem(ctx, task, a, out.Limits, usage)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, item)
	}
	// Digest flags: the run's template differs from the policy's at lease
	// time, or from every template the other arms' items ran with.
	digests := map[string]map[string]bool{}
	for _, it := range out.Items {
		if digests[it.Arm] == nil {
			digests[it.Arm] = map[string]bool{}
		}
		digests[it.Arm][it.HandlerDigest] = true
	}
	for i := range out.Items {
		it := &out.Items[i]
		if it.HandlerDigest == "" || it.HandlerDigest != assignments[i].PolicyDigest {
			it.DigestFlags = append(it.DigestFlags, "policy")
		}
		others, shared := 0, false
		for arm, set := range digests {
			if arm == it.Arm {
				continue
			}
			others++
			shared = shared || set[it.HandlerDigest]
		}
		if others > 0 && !shared {
			it.DigestFlags = append(it.DigestFlags, "other_arm")
		}
	}
	rotations, err := s.armRotations(ctx, task, out.Policy)
	if err != nil {
		return out, err
	}
	out.Arms = handlerABArms(out.Policy, out.Items, rotations)
	out.Comparisons = handlerABComparisons(out.Arms, out.Items)
	return out, nil
}

func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i := range parts {
		parts[i] = prefix + parts[i]
	}
	return strings.Join(parts, ",")
}

func (s *Store) handlerABItem(ctx context.Context, task string, a armAssignmentRow, limits []api.HandlerArmLimit, usage api.UsageReport) (api.HandlerABItem, error) {
	it := api.HandlerABItem{EntryID: a.entryID, ItemID: a.itemID, Arm: a.Arm, DrawnArm: a.DrawnArm, Fallback: a.Fallback, HandlerID: a.HandlerID,
		HandlerRunID: a.HandlerRunID, HandlerDigest: a.HandlerDigest, DigestFlags: []string{}, LeasedAt: a.LeasedAt, FinishedAt: a.FinishedAt,
		HandlerAllocatedTurns: "0", ResponseMillis: []int64{}, Interventions: map[string]int{}}
	leased, finished := parseTS(a.LeasedAt), parseTS(a.FinishedAt)
	if a.FinishedAt != "" {
		ms := finished.Sub(leased).Milliseconds()
		it.EntryFinishedMillis = &ms
	}
	windowEnd := finished
	if a.FinishedAt == "" {
		windowEnd = s.now()
	}
	inWindow := func(at time.Time) bool { return !at.Before(leased) && !at.After(windowEnd) }
	if err := s.db.QueryRowContext(ctx, `SELECT title FROM work_items WHERE task_id=? AND id=?`, task, a.itemID).Scan(&it.Title); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return it, err
	}
	// Handler tokens: the item's database_handler role group.
	for _, report := range usage.Items {
		if report.ItemID != a.itemID {
			continue
		}
		for _, g := range report.Roles {
			if g.Key != api.AgentRoleDatabaseHandler {
				continue
			}
			summary := g.Summary
			it.HandlerUsage = &summary
			total := new(big.Rat)
			for _, v := range summary.Tokens {
				total.Add(total, parseRat(v))
			}
			it.HandlerTokens = new(big.Int).Quo(total.Num(), total.Denom()).Int64()
			input := new(big.Rat).Add(parseRat(summary.Tokens["input"]), parseRat(summary.Tokens["cached"]))
			it.HandlerInputTokens = new(big.Int).Quo(input.Num(), input.Denom()).Int64()
			it.HandlerRequests = summary.Requests
			it.HandlerAllocatedTurns = summary.AllocatedTurns
			if it.HandlerAllocatedTurns == "" {
				it.HandlerAllocatedTurns = "0"
			}
		}
	}
	chain, err := handlerChain(ctx, s.db, a.HandlerID)
	if err != nil {
		return it, err
	}
	// Response times: closed handler-request obligations with a primary link
	// to the item, answered by result or decline.
	rows, err := s.db.QueryContext(ctx, `SELECT o.created_at,o.closed_at FROM obligations o
 JOIN message_work_item_links l ON l.message_task_id=o.task_id AND l.message_seq=o.message_seq
 WHERE o.task_id=? AND `+inList("o.agent_id", len(chain))+` AND o.source_kind=? AND o.needs=? AND o.state=? AND o.outcome IN (?,?) AND l.item_id=? ORDER BY o.message_seq`,
		chainArgs([]any{task}, chain, api.EnvelopeKindRequest, api.ObligationNeedsOutcome, api.ObligationClosed, api.OutcomeResult, api.OutcomeDeclined, a.itemID)...)
	if err != nil {
		return it, err
	}
	for rows.Next() {
		var created, closed string
		if err := rows.Scan(&created, &closed); err != nil {
			rows.Close()
			return it, err
		}
		if closed != "" {
			it.ResponseMillis = append(it.ResponseMillis, max(0, parseTS(closed).Sub(parseTS(created)).Milliseconds()))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return it, err
	}
	// Launch to done ends at the item's first done revision after the lease.
	type revision struct {
		number                         int64
		status, agent, run, node, prov string
		at                             time.Time
		changed                        map[string]bool
		checkpoint                     bool
	}
	var revisions []revision
	rows, err = s.db.QueryContext(ctx, `SELECT revision,status,updated_agent,updated_run_id,updated_node,provenance,updated_at,COALESCE(changed_fields,'') FROM work_item_revisions WHERE item_task_id=? AND item_id=? ORDER BY revision`, task, a.itemID)
	if err != nil {
		return it, err
	}
	for rows.Next() {
		var r revision
		var at, changed string
		if err := rows.Scan(&r.number, &r.status, &r.agent, &r.run, &r.node, &r.prov, &at, &changed); err != nil {
			rows.Close()
			return it, err
		}
		r.at = parseTS(at)
		r.changed = map[string]bool{}
		if changed == "" {
			r.checkpoint = true
		} else {
			var fields []string
			if json.Unmarshal([]byte(changed), &fields) == nil {
				for _, f := range fields {
					r.changed[f] = true
				}
			}
		}
		revisions = append(revisions, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return it, err
	}
	for _, r := range revisions {
		if r.status == "done" && !r.at.Before(leased) {
			ms := r.at.Sub(leased).Milliseconds()
			it.LaunchToDoneMillis = &ms
			break
		}
	}
	// Owner corrections: a handler revision in the lease window whose fields
	// a later owner-attributed revision changes again. Each counts once.
	inChain := map[string]bool{}
	for _, id := range chain {
		inChain[id] = true
	}
	helpers, err := ownerHelperAgents(ctx, s.db, task)
	if err != nil {
		return it, err
	}
	// Owner-attributed: a native write by the owner's own session (the owner
	// helper) or by a person with no agent. Hub-internal system callers,
	// reconstructed history and checkpoints are not owner writes.
	owner := func(r revision) bool {
		if r.checkpoint || r.prov != "native" {
			return false
		}
		if r.agent == "" {
			return !handlerABSystemNodes[r.node]
		}
		return helpers[r.agent]
	}
	for i, h := range revisions {
		if h.checkpoint || !inChain[h.agent] || !inWindow(h.at) {
			continue
		}
	later:
		for _, o := range revisions[i+1:] {
			if !owner(o) {
				continue
			}
			for f := range h.changed {
				if o.changed[f] {
					it.OwnerCorrections++
					break later
				}
			}
		}
	}
	queries := []struct {
		dest  *int
		query string
		args  []any
	}{
		{&it.HandlerBlocks, `SELECT count(*) FROM messages m JOIN message_work_item_links l ON l.message_task_id=m.task_id AND l.message_seq=m.seq
 WHERE m.task_id=? AND l.item_id=? AND m.envelope<>'' AND json_extract(m.envelope,'$.kind')='block' AND ` + inList("m.to_agent", len(chain)), chainArgs([]any{task, a.itemID}, chain)},
		{&it.HandlerAuthoredBlocks, `SELECT count(*) FROM messages m JOIN message_work_item_links l ON l.message_task_id=m.task_id AND l.message_seq=m.seq
 WHERE m.task_id=? AND l.item_id=? AND m.envelope<>'' AND json_extract(m.envelope,'$.kind')='block' AND ` + inList("m.from_agent", len(chain)), chainArgs([]any{task, a.itemID}, chain)},
		{&it.GateFixes, `SELECT count(*) FROM owner_interventions WHERE task_id=? AND item_id=? AND kind='gate-fix'`, []any{task, a.itemID}},
		{&it.LinkCorrections, `SELECT count(*) FROM message_audit_events ev JOIN messages m ON m.task_id=ev.message_task_id AND m.seq=ev.message_seq
 JOIN message_work_item_links l ON l.message_task_id=m.task_id AND l.message_seq=m.seq
 WHERE ev.message_task_id=? AND ev.operation='correct' AND l.item_id=? AND ` + inList("m.from_agent", len(chain)), chainArgs([]any{task, a.itemID}, chain)},
	}
	for _, c := range queries {
		if *c.dest, err = countQuery(ctx, s.db, c.query, c.args...); err != nil {
			return it, err
		}
	}
	it.IncorrectSaves = it.OwnerCorrections + it.GateFixes
	// Refused saves by the leased handler run(s) within the lease window.
	rows, err = s.db.QueryContext(ctx, `SELECT at FROM handler_write_refusals WHERE task_id=? AND item_id=? AND `+inList("agent_id", len(chain)), chainArgs([]any{task, a.itemID}, chain)...)
	if err != nil {
		return it, err
	}
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			rows.Close()
			return it, err
		}
		if inWindow(parseTS(at)) {
			it.RefusedSaves++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return it, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT kind,count(*) FROM owner_interventions WHERE task_id=? AND item_id=? GROUP BY kind ORDER BY kind`, task, a.itemID)
	if err != nil {
		return it, err
	}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			rows.Close()
			return it, err
		}
		it.Interventions[kind] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return it, err
	}
	// Limit events: the arm's episodes overlapping the lease, and the two
	// Codex prompt escalations of the handler runs within it.
	for _, ep := range limits {
		if ep.Arm != a.Arm || ep.StartedAt.After(windowEnd) || (ep.ClearedAt != nil && ep.ClearedAt.Before(leased)) {
			continue
		}
		it.LimitEvents.Episodes++
	}
	rows, err = s.db.QueryContext(ctx, `SELECT kind,created_at FROM runtime_prompt_escalations WHERE task_id=? AND kind IN (?,?) AND `+inList("agent_id", len(chain)),
		chainArgs([]any{task, api.RuntimePromptCodexUsageLimit, api.RuntimePromptCodexRateLimit}, chain)...)
	if err != nil {
		return it, err
	}
	for rows.Next() {
		var kind, at string
		if err := rows.Scan(&kind, &at); err != nil {
			rows.Close()
			return it, err
		}
		if !inWindow(parseTS(at)) {
			continue
		}
		if kind == api.RuntimePromptCodexUsageLimit {
			it.LimitEvents.CodexUsageLimit++
		} else {
			it.LimitEvents.CodexRateLimit++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return it, err
	}
	it.LimitEvents.Total = it.LimitEvents.Episodes + it.LimitEvents.CodexUsageLimit + it.LimitEvents.CodexRateLimit
	return it, nil
}

// handlerABSystemNodes are the caller nodes hub-internal writers use; their
// agentless writes are not the owner's.
var handlerABSystemNodes = map[string]bool{"system": true, "team_queue": true, "handler_arms": true}

func ownerHelperAgents(ctx context.Context, q queryRower, task string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM agents WHERE task_id=? AND role=?`, task, api.AgentRoleOwnerHelper)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// armRotations counts committed rotations whose old run matched each arm.
func (s *Store) armRotations(ctx context.Context, task string, p api.HandlerArmPolicy) (map[string]int, error) {
	out := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT r.old_agent_id,r.old_run_id,a.runtime FROM handler_rotations r JOIN agents a ON a.id=r.old_agent_id WHERE r.task_id=? AND r.state='committed'`, task)
	if err != nil {
		return nil, err
	}
	type old struct{ agent, run, runtime string }
	var olds []old
	for rows.Next() {
		var o old
		if err := rows.Scan(&o.agent, &o.run, &o.runtime); err != nil {
			rows.Close()
			return nil, err
		}
		olds = append(olds, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, o := range olds {
		rec, err := loadHandlerRunRecord(ctx, s.db, o.agent, o.run)
		if err != nil {
			return nil, err
		}
		if arm := handlerArmFor(p, o.runtime, rec.model, rec.reasoning); arm != "" {
			out[arm]++
		}
	}
	return out, nil
}

func handlerABArms(p api.HandlerArmPolicy, items []api.HandlerABItem, rotations map[string]int) []api.HandlerABArm {
	var arms []api.HandlerABArm
	known := map[string]int{}
	for _, arm := range sortedArms(p.Arms) {
		known[arm.ID] = len(arms)
		arms = append(arms, api.HandlerABArm{Arm: arm.ID, Runtime: arm.Runtime, Model: arm.Model, Reasoning: arm.Reasoning})
	}
	var extra []string
	for _, it := range items {
		if _, ok := known[it.Arm]; !ok {
			known[it.Arm] = -1
			extra = append(extra, it.Arm)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		known[id] = len(arms)
		arms = append(arms, api.HandlerABArm{Arm: id})
	}
	for i := range arms {
		a := &arms[i]
		var responses, done, tokens []int64
		input, turns := new(big.Rat), new(big.Rat)
		totals := map[string]int{}
		for _, it := range items {
			if it.Arm != a.Arm {
				continue
			}
			a.N++
			if it.Fallback {
				a.Fallbacks++
			}
			responses = append(responses, it.ResponseMillis...)
			if it.LaunchToDoneMillis != nil {
				done = append(done, *it.LaunchToDoneMillis)
			}
			tokens = append(tokens, it.HandlerTokens)
			input.Add(input, new(big.Rat).SetInt64(it.HandlerInputTokens))
			turns.Add(turns, parseRat(it.HandlerAllocatedTurns))
			for k, v := range abItemCounts(it) {
				totals[k] += v
			}
			if len(it.DigestFlags) > 0 {
				a.DigestFlaggedItems++
			}
		}
		a.Requests = len(responses)
		a.ResponseMedianMillis, a.ResponseP90Millis = abMedian(responses), abP90(responses)
		a.LaunchToDoneMedian, a.LaunchToDoneDone = abMedian(done), len(done)
		a.HandlerTokensMedian = abMedian(tokens)
		if turns.Sign() > 0 {
			mean := ratString(new(big.Rat).Quo(input, turns))
			a.MeanInputPerRequest = &mean
		}
		a.Rotations = rotations[a.Arm]
		a.Counts = map[string]api.HandlerABCount{}
		for _, k := range abCountKeys {
			c := api.HandlerABCount{Total: totals[k], Rate: "0"}
			if a.N > 0 {
				c.Rate = ratString(big.NewRat(int64(totals[k]), int64(a.N)))
			}
			a.Counts[k] = c
		}
	}
	return arms
}

func handlerABComparisons(arms []api.HandlerABArm, items []api.HandlerABItem) []api.HandlerABComparison {
	samples := func(arm string, pick func(api.HandlerABItem) []int64) []int64 {
		var out []int64
		for _, it := range items {
			if it.Arm == arm {
				out = append(out, pick(it)...)
			}
		}
		return out
	}
	metrics := []struct {
		name string
		pick func(api.HandlerABItem) []int64
	}{
		{"responseMillis", func(it api.HandlerABItem) []int64 { return it.ResponseMillis }},
		{"launchToDoneMillis", func(it api.HandlerABItem) []int64 {
			if it.LaunchToDoneMillis == nil {
				return nil
			}
			return []int64{*it.LaunchToDoneMillis}
		}},
		{"handlerTokens", func(it api.HandlerABItem) []int64 { return []int64{it.HandlerTokens} }},
	}
	out := []api.HandlerABComparison{}
	for i := 0; i < len(arms); i++ {
		for j := i + 1; j < len(arms); j++ {
			a, b := arms[i], arms[j]
			for _, m := range metrics {
				out = append(out, api.HandlerABComparison{Metric: m.name, A: a.Arm, B: b.Arm, Flag: abNoiseFlag(a.N, b.N, samples(a.Arm, m.pick), samples(b.Arm, m.pick))})
			}
			for _, k := range abCountKeys {
				out = append(out, api.HandlerABComparison{Metric: k, A: a.Arm, B: b.Arm, Flag: abCountFlag(a.N, b.N, a.Counts[k].Total, b.Counts[k].Total)})
			}
		}
	}
	return out
}

// Automatic handler provisioning (docs/handler-ab.md, "Automatic
// provisioning"). A queued entry that is admissible except for a free
// database handler gets a HandlerNeed. When the project's switch is on, the
// queue limit has room for another handler of the wanted settings and the
// handler plus the waiting team fit under the project agent cap, the runner
// reserves one with provision_handler and starts it from its saved launch
// spec. The store decides the exact match of that spec itself.

const noFreeHandlerReason = "No free database handler"

// handlerProvisionAbandonAfter is how long a reserved handler may take to
// register online before its reservation stops holding the next one.
const handlerProvisionAbandonAfter = 10 * time.Minute

const handlerProvisionNoticeSubject = "Automatic handler provision"

// queueHandlerProvision reads the project's automatic handler provisioning
// switch. A project with no settings row reads on.
func queueHandlerProvision(ctx context.Context, q queryRower, task string) (bool, error) {
	var on bool
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT handler_provision FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&on)
	return on, err
}

func handlerProvisionState(on bool) string {
	if on {
		return api.HandlerProvisionOn
	}
	return api.HandlerProvisionOff
}

type handlerProvisionRow struct {
	id, entryID, arm, agentID, state, reason, host    string
	specRuntime, specModel, specReasoning, specDigest string
	fix, createdAt                                    string
}

const handlerProvisionCols = `id,entry_id,arm,agent_id,state,reason,host,spec_runtime,spec_model,spec_reasoning,spec_digest,fix,created_at`

func scanHandlerProvision(row interface{ Scan(...any) error }) (handlerProvisionRow, error) {
	var r handlerProvisionRow
	err := row.Scan(&r.id, &r.entryID, &r.arm, &r.agentID, &r.state, &r.reason, &r.host, &r.specRuntime, &r.specModel, &r.specReasoning, &r.specDigest, &r.fix, &r.createdAt)
	return r, err
}

// handlerRegistered reports whether a reserved handler agent has registered
// and is an online, usable database handler.
func handlerRegistered(ctx context.Context, q queryRower, agentID string) (bool, error) {
	a, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, agentID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.Role == api.AgentRoleDatabaseHandler && a.Online && a.Status != api.AgentClosed && a.Status != api.AgentExited && a.Status != api.AgentRetired, nil
}

// reservedHandlerProvision reads the project's reserved row, if any, and what
// it has become: "registered", "abandoned" or still "reserved". It writes
// nothing; settleHandlerProvisions saves the same judgement.
func reservedHandlerProvision(ctx context.Context, q queryRower, task string, now time.Time) (*handlerProvisionRow, string, error) {
	r, err := scanHandlerProvision(q.QueryRowContext(ctx, `SELECT `+handlerProvisionCols+` FROM handler_provisions WHERE task_id=? AND state='reserved'`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	registered, err := handlerRegistered(ctx, q, r.agentID)
	if err != nil {
		return nil, "", err
	}
	switch {
	case registered:
		return &r, "registered", nil
	case !now.Before(parseTS(r.createdAt).Add(handlerProvisionAbandonAfter)):
		return &r, "abandoned", nil
	}
	return &r, "reserved", nil
}

// settleHandlerProvisions saves a reserved row's outcome: registered once its
// handler is online, abandoned after ten minutes otherwise.
func settleHandlerProvisions(ctx context.Context, tx *sql.Tx, task string, now time.Time) error {
	r, state, err := reservedHandlerProvision(ctx, tx, task, now)
	if err != nil || r == nil || state == "reserved" {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE handler_provisions SET state=?,updated_at=? WHERE id=? AND state='reserved'`, state, ts(now), r.id)
	return err
}

// clearHandlerProvisionRefusal drops an entry's standing spec refusal, as
// when the entry leases a handler after all.
func clearHandlerProvisionRefusal(ctx context.Context, tx *sql.Tx, entryID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM handler_provisions WHERE entry_id=? AND state='refused'`, entryID)
	return err
}

// handlerNeedContext is what every handler need of one listing or one
// provision shares: the open handlers, the cap counts and the pending
// reservation are read once.
type handlerNeedContext struct {
	policy            api.HandlerArmPolicy
	active            []api.TeamQueueEntry
	limit, maxAgents  int
	autoOn            bool
	now               time.Time
	handlers          []api.HandlerArmHandler
	capOpen, capSeats int
	pending           *handlerProvisionRow
	// over is a reserved row that has become registered or abandoned but is
	// not saved as such yet; it already counts as a finished attempt.
	over               *handlerProvisionRow
	leasedByActive     map[string]bool
	effectiveLimit     int
	effectiveLimitText string
	// primaryID is the project's recorded primary handler, which the handler
	// floor lets an operator rotate but not retire.
	primaryID string
}

func loadHandlerNeedContext(ctx context.Context, q queryRower, p api.HandlerArmPolicy, active []api.TeamQueueEntry, limit, maxAgents int, now time.Time) (*handlerNeedContext, error) {
	c := &handlerNeedContext{policy: p, active: active, limit: limit, maxAgents: maxAgents, now: now, leasedByActive: map[string]bool{}}
	var err error
	if c.autoOn, err = queueHandlerProvision(ctx, q, p.TaskID); err != nil {
		return nil, err
	}
	all, err := armHandlers(ctx, q, p)
	if err != nil {
		return nil, err
	}
	for _, h := range all {
		if h.Status != api.AgentRetired {
			c.handlers = append(c.handlers, h)
		}
	}
	if c.capOpen, c.capSeats, err = projectAgentCapUsage(ctx, q, p.TaskID); err != nil {
		return nil, err
	}
	if err = q.QueryRowContext(ctx, `SELECT primary_handler_id FROM tasks WHERE id=?`, p.TaskID).Scan(&c.primaryID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	pending, state, err := reservedHandlerProvision(ctx, q, p.TaskID, now)
	if err != nil {
		return nil, err
	}
	if state == "reserved" {
		c.pending = pending
	} else {
		c.over = pending
	}
	for _, e := range active {
		if e.HandlerID != "" {
			c.leasedByActive[e.HandlerID] = true
		}
	}
	// With no fixed cap the queue wants one handler per active team and one
	// for the team that waits.
	c.effectiveLimit, c.effectiveLimitText = limit, fmt.Sprint(limit)
	if limit == 0 {
		c.effectiveLimit, c.effectiveLimitText = len(active)+1, "none"
	}
	return c, nil
}

// need computes the handler need of a queued entry whose only block is the
// handler. drawn is its arm under an enabled policy, else empty: the wanted
// settings are then the first open handler's recorded ones, and a project
// whose first handler recorded no template has no need to state (it keeps the
// legacy reason). The second result is the text to append to the entry's
// wait reason. ignoreRefusal leaves a standing spec refusal out, for the
// provision that re-examines the spec. It writes nothing.
func (c *handlerNeedContext) need(ctx context.Context, q queryRower, e api.TeamQueueEntry, drawn string, ignoreRefusal bool) (*api.TeamQueueHandlerNeed, string, error) {
	n := &api.TeamQueueHandlerNeed{Arm: drawn}
	if drawn != "" {
		for _, arm := range c.policy.Arms {
			if arm.ID == drawn {
				n.Runtime, n.Model, n.Reasoning, n.TemplateDigest = arm.Runtime, arm.Model, arm.Reasoning, c.policy.TemplateDigest
			}
		}
	} else {
		if len(c.handlers) == 0 || c.handlers[0].Model == "" || c.handlers[0].TemplateDigest == "" {
			return nil, "", nil
		}
		first := c.handlers[0]
		n.Runtime, n.Model, n.Reasoning, n.TemplateDigest = first.Runtime, first.Model, first.Reasoning, first.TemplateDigest
	}
	var offline []api.HandlerArmHandler
	for _, h := range c.handlers {
		if h.Runtime != n.Runtime || h.Model != n.Model || h.Reasoning != n.Reasoning {
			continue
		}
		n.Handlers++
		if c.leasedByActive[h.AgentID] {
			n.Leased++
		}
		if !h.Online {
			offline = append(offline, h)
		}
	}
	if n.Handlers >= c.effectiveLimit {
		// The limit already has its handlers, and another one would not be
		// within the limit. Busy ones free themselves: the reason stays the
		// plain wait. An offline one does not come back by itself, so the
		// reason names it and the command an operator runs; the hub runs
		// nothing.
		n.Reason = fmt.Sprintf("%d of %d leased, limit %s; the limit already has its handlers", n.Leased, n.Handlers, c.effectiveLimitText)
		if len(offline) == 0 {
			return n, "", nil
		}
		// Either command frees a place; the retire is the lighter one, so a
		// handler that is not the primary comes first.
		first := offline[0]
		for _, h := range offline {
			if h.AgentID != c.primaryID {
				first = h
				break
			}
		}
		var recovery string
		n.Fix, recovery = c.offlineHandlerFix(e.TaskID, first)
		n.Reason += fmt.Sprintf(", and %s. The hub does not restart a handler; an operator can %s. Fix: %s", offlineHandlerNames(offline), recovery, n.Fix)
		return n, ": " + n.Reason, nil
	}
	counts := fmt.Sprintf("%d of %d leased, limit %s", n.Leased, n.Handlers, c.effectiveLimitText)
	lower := fmt.Sprintf("tt team queue limit --task %s --limit %d", e.TaskID, n.Handlers)
	var attempts int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM handler_provisions WHERE entry_id=? AND state IN ('registered','abandoned')`, e.ID).Scan(&attempts); err != nil {
		return nil, "", err
	}
	if c.over != nil && c.over.entryID == e.ID {
		// The listing judges an expired reservation without saving it, so the
		// runner's next retry identity is already the next attempt; that
		// request then saves the outcome and reserves anew.
		attempts++
	}
	n.Attempt = attempts + 1
	kind, err := queueItemKind(ctx, q, e.TaskID, e.ItemID)
	if err != nil {
		return nil, "", err
	}
	seats := queueTeamSeats(kind, e.Template)
	switch {
	case !c.autoOn:
		n.Fix = fmt.Sprintf("tt team queue provision --task %s --auto on", e.TaskID)
		n.Reason = counts + "; automatic provisioning is off. Fix: " + n.Fix
	case c.pending != nil:
		if c.pending.entryID == e.ID {
			n.AgentID = c.pending.agentID
		}
		n.Reason = counts + "; the runner is adding one"
	case c.capOpen+c.capSeats+seats+1 > c.maxAgents:
		// The new handler and the waiting team must both fit, so a provision
		// is never what pushes the team over the cap.
		reserved := ""
		if c.capSeats > 0 {
			reserved = fmt.Sprintf(" + %d reserved", c.capSeats)
		}
		n.Fix = lower
		n.Reason = fmt.Sprintf("%s; cannot add one: %s%d open%s + %d seats + 1 handler > %d. Fix: %s", counts, projectAgentCapPrefix, c.capOpen, reserved, seats, c.maxAgents, n.Fix)
	default:
		n.Provision = true
		n.Reason = counts + "; the runner is adding one"
		if ignoreRefusal {
			break
		}
		refused, err := scanHandlerProvision(q.QueryRowContext(ctx, `SELECT `+handlerProvisionCols+` FROM handler_provisions WHERE entry_id=? AND state='refused'`, e.ID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, "", err
		}
		if err == nil {
			n.Provision, n.Refused = false, true
			n.Fix = refused.fix
			n.Reason = counts + "; " + refused.reason + ", or: " + lower
		}
	}
	return n, ": " + n.Reason, nil
}

// offlineHandlerFix is the command an operator runs for an offline handler
// that holds a place in the limit, and what it does. A retired handler no
// longer counts, so the need falls below the limit and the provisioning rules
// apply. The handler floor refuses to retire the primary, which is rotated.
func (c *handlerNeedContext) offlineHandlerFix(task string, h api.HandlerArmHandler) (fix, recovery string) {
	if h.AgentID == c.primaryID {
		return fmt.Sprintf("tt handler rotate --task %s", task), fmt.Sprintf("rotate %s, the primary, to a successor", provisionText(h.Name))
	}
	name := h.Name
	if !api.ValidName(name) || strings.HasPrefix(name, "-") {
		// tt retire also resolves an agent ID, which no shell or flag parser
		// misreads.
		name = h.AgentID
	}
	return fmt.Sprintf("tt retire --task %s %s", task, name), fmt.Sprintf("retire %s so that it stops counting and one can be added", provisionText(h.Name))
}

// offlineHandlerNames lists offline handlers in lease order: "a is offline",
// "a and b are offline".
func offlineHandlerNames(offline []api.HandlerArmHandler) string {
	names := make([]string, len(offline))
	for i, h := range offline {
		names[i] = provisionText(h.Name)
	}
	if len(names) == 1 {
		return names[0] + " is offline"
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " are offline"
}

// provisionText makes a runner-supplied value safe to show in a reason: one
// line, no control characters, bounded.
func provisionText(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, v)
	if len(v) > 200 {
		v = v[:200] + "..."
	}
	return v
}

func handlerSpecText(runtime, model, reasoning, digest string) string {
	if runtime == "" && model == "" && reasoning == "" && digest == "" {
		return "missing"
	}
	return fmt.Sprintf("%s/%s/%s digest %s", or(provisionText(runtime), "-"), or(provisionText(model), "-"), or(provisionText(reasoning), "-"), or(provisionText(digest), "-"))
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// handlerSpecKeptFlags are the launch flags a refusal's command carries over
// from the saved spec; the wanted runtime, model and reasoning and the prompt
// replace the saved ones.
var handlerSpecKeptFlags = map[string]bool{"--run": true, "--cwd": true, "--permission-mode": true, "--approval-mode": true, "--sandbox-mode": true, "--allowed-tools-json": true}

// handlerSpecFix is the command that saves a launch spec with the wanted
// settings, and a note on what it leaves to the owner. tt handler spec
// replaces the whole spec, so the saved spec's other flags are repeated. The
// hub holds only the prompt's digest: PROMPT_FILE is the one placeholder, and
// the command stops before saving anything if that file cannot be read. When
// the saved run command is unknown or starts another runtime, --run is left
// out, and tt handler spec refuses the command until it is added.
func handlerSpecFix(task string, n api.TeamQueueHandlerNeed, req api.TeamQueueRequest) (string, string) {
	kept, hasRun := "", false
	if req.HandlerSpecRuntime == n.Runtime && len(req.HandlerSpecArgs)%2 == 0 && len(req.HandlerSpecArgs) <= 2*len(handlerSpecKeptFlags) {
		for i := 0; i+1 < len(req.HandlerSpecArgs); i += 2 {
			flag, value := req.HandlerSpecArgs[i], req.HandlerSpecArgs[i+1]
			if !handlerSpecKeptFlags[flag] || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
				kept, hasRun = "", false
				break
			}
			kept += " " + flag + " " + shellQuote(value)
			hasRun = hasRun || flag == "--run"
		}
	}
	if !hasRun {
		kept = ""
	}
	fix := fmt.Sprintf(`PROMPT="$(cat PROMPT_FILE)" && tt handler spec --task %s --%s --runtime %s --model %s --reasoning %s --prompt "$PROMPT"`, task, kept, n.Runtime, n.Model, n.Reasoning)
	note := "PROMPT_FILE holds the handler prompt with that template digest"
	if !hasRun {
		note += "; add --run with the " + n.Runtime + " launch command and the host's other launch flags, tt refuses the spec without --run"
	}
	return fix, note
}

// handlerSpecRefusal is the stored reason and fix of a spec that does not
// match.
func handlerSpecRefusal(task, host string, req api.TeamQueueRequest, n api.TeamQueueHandlerNeed) (string, string) {
	wanted := "the handlers in use"
	if n.Arm != "" {
		wanted = "arm " + n.Arm
	}
	fix, note := handlerSpecFix(task, n, req)
	return fmt.Sprintf("cannot add one: the saved launch spec on %s is %s, %s needs %s. Fix: %s (%s)",
		provisionText(host), handlerSpecText(req.HandlerSpecRuntime, req.HandlerSpecModel, req.HandlerSpecReasoning, req.HandlerSpecDigest), wanted,
		handlerSpecText(n.Runtime, n.Model, n.Reasoning, n.TemplateDigest), fix, note), fix
}

// provisionHandler is the provision_handler queue operation: it recomputes
// the entry's handler need and, when one may be added and the runner's saved
// spec matches the wanted runtime, model, reasoning and template digest
// exactly, reserves the preallocated handler and posts one Board notice. A
// spec that differs or is missing stores one refused row for the entry; the
// caller commits it and returns the second result as the 409.
func (s *Store) provisionHandler(ctx context.Context, tx *sql.Tx, t api.Task, req api.TeamQueueRequest) (api.TeamQueueEntry, error, error) {
	var zero api.TeamQueueEntry
	// The spec values are whatever the host has saved. A value no handler
	// could have is not a malformed request: it is a spec that does not match,
	// and the entry's reason must say so.
	if !api.ValidID(req.HandlerAgentID, "agt") || req.Host == "" || req.EntryID == "" {
		return zero, nil, api.ErrInvalid
	}
	now := s.now()
	e, err := scanTeamQueue(tx.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND id=?`, t.ID, req.EntryID))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, nil, api.ErrNotFound
	}
	if err != nil {
		return zero, nil, err
	}
	if e.State != "queued" || (req.ExpectedRevision != 0 && req.ExpectedRevision != e.Revision) {
		return zero, nil, fmt.Errorf("%w: entry revision changed", api.ErrConflict)
	}
	if t.PauseState != api.ProjectPauseActive {
		return zero, nil, fmt.Errorf("%w: project is not launchable", api.ErrConflict)
	}
	if err := settleHandlerProvisions(ctx, tx, t.ID, now); err != nil {
		return zero, nil, err
	}
	limit, err := queueConcurrencyLimit(ctx, tx, t.ID)
	if err != nil {
		return zero, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND `+queueHoldsSQL+` ORDER BY position`, t.ID)
	if err != nil {
		return zero, nil, err
	}
	var active []api.TeamQueueEntry
	for rows.Next() {
		a, scanErr := scanTeamQueue(rows)
		if scanErr != nil {
			rows.Close()
			return zero, nil, scanErr
		}
		active = append(active, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return zero, nil, err
	}
	// The entry must be admissible except for the handler, in the claim's
	// order: slots, host admission, then nothing active in its way.
	if queueSlotsFull(limit, len(active)) {
		return zero, nil, fmt.Errorf("%w: all team slots are reserved", api.ErrConflict)
	}
	if queueParallel(limit) {
		if err := checkTeamHostAdmission(ctx, tx, e.Host, now); err != nil {
			return zero, nil, err
		}
	}
	for _, a := range active {
		if e.Cwd == a.Cwd || queueEntryConflicts(e, a) {
			return zero, nil, fmt.Errorf("%w: the entry waits for an active team, not for a handler", api.ErrConflict)
		}
	}
	p, err := loadHandlerArmPolicy(ctx, tx, t.ID)
	if err != nil {
		return zero, nil, err
	}
	drawn := ""
	if p.Enabled {
		d, err := decideHandlerArm(ctx, tx, p, e.ID, active, now)
		if err != nil {
			return zero, nil, err
		}
		if d.wait == "" || d.wait == armAllLimitedMessage {
			return zero, nil, fmt.Errorf("%w: the entry does not wait for a handler that can be added", api.ErrConflict)
		}
		drawn = d.drawn
	} else {
		free, err := freeQueueHandler(ctx, tx, t.ID, active)
		if err != nil {
			return zero, nil, err
		}
		if free.ID != "" {
			return zero, nil, fmt.Errorf("%w: the entry does not wait for a handler that can be added", api.ErrConflict)
		}
	}
	nc, err := loadHandlerNeedContext(ctx, tx, p, active, limit, s.MaxAgents, now)
	if err != nil {
		return zero, nil, err
	}
	need, _, err := nc.need(ctx, tx, e, drawn, true)
	if err != nil {
		return zero, nil, err
	}
	if need == nil {
		return zero, nil, fmt.Errorf("%w: the project's handlers recorded no launch template, so none can be added automatically", api.ErrConflict)
	}
	if !need.Provision {
		return zero, nil, fmt.Errorf("%w: no handler is added: %s", api.ErrConflict, need.Reason)
	}
	stamp := ts(now)
	if req.HandlerSpecRuntime != need.Runtime || req.HandlerSpecModel != need.Model || req.HandlerSpecReasoning != need.Reasoning || req.HandlerSpecDigest != need.TemplateDigest {
		reason, fix := handlerSpecRefusal(t.ID, req.Host, req, *need)
		refusal := fmt.Errorf("%w: no handler is added: %s", api.ErrConflict, reason)
		standing, err := scanHandlerProvision(tx.QueryRowContext(ctx, `SELECT `+handlerProvisionCols+` FROM handler_provisions WHERE entry_id=? AND state='refused'`, e.ID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return zero, nil, err
		}
		if err == nil && standing.reason == reason && standing.fix == fix {
			// The same spec offered again: the standing row already says
			// this, so nothing is written and nobody is notified.
			return zero, nil, refusal
		}
		if err := clearHandlerProvisionRefusal(ctx, tx, e.ID); err != nil {
			return zero, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO handler_provisions(id,task_id,entry_id,arm,agent_id,state,reason,request_id,host,spec_runtime,spec_model,spec_reasoning,spec_digest,fix,created_at,updated_at)
 VALUES(?,?,?,?,'','refused',?,?,?,?,?,?,?,?,?,?)`, api.NewID("hpv"), t.ID, e.ID, need.Arm, reason, req.RequestID, provisionText(req.Host),
			provisionText(req.HandlerSpecRuntime), provisionText(req.HandlerSpecModel), provisionText(req.HandlerSpecReasoning), provisionText(req.HandlerSpecDigest), fix, stamp, stamp); err != nil {
			return zero, nil, err
		}
		return zero, refusal, nil
	}
	if err := clearHandlerProvisionRefusal(ctx, tx, e.ID); err != nil {
		return zero, nil, err
	}
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE id=?`, req.HandlerAgentID).Scan(&taken); err != nil {
		return zero, nil, err
	}
	if taken != 0 {
		return zero, nil, fmt.Errorf("%w: the preallocated handler agent already exists", api.ErrConflict)
	}
	armText, refs := "", map[string]string{"entry": e.ID, "agent": req.HandlerAgentID, "item": e.ItemID}
	if need.Arm != "" {
		armText, refs["arm"] = " of arm "+need.Arm, need.Arm
	}
	env := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: handlerProvisionNoticeSubject, Refs: refs, Body: api.EnvelopeBody{Text: fmt.Sprintf(
		"The team queue is adding one database handler%s (%s, %s/%s/%s) on %s for waiting entry %s: %d of %d handlers are leased and the queue limit is %s. This is an automatic provision, not an owner intervention. Turn it off with: tt team queue provision --task %s --auto off",
		armText, req.HandlerAgentID, need.Runtime, need.Model, need.Reasoning, req.Host, e.ID, need.Leased, need.Handlers, nc.effectiveLimitText, t.ID)}}
	m, err := s.insertMessage(ctx, tx, t, api.PostMessageRequest{Envelope: &env}, api.Agent{}, api.Caller{Node: "team_queue", User: "runner"}, false, false)
	if err != nil {
		return zero, nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO handler_provisions(id,task_id,entry_id,arm,agent_id,state,request_id,host,spec_runtime,spec_model,spec_reasoning,spec_digest,notice_seq,created_at,updated_at)
 VALUES(?,?,?,?,?,'reserved',?,?,?,?,?,?,?,?,?)`, api.NewID("hpv"), t.ID, e.ID, need.Arm, req.HandlerAgentID, req.RequestID, req.Host,
		req.HandlerSpecRuntime, req.HandlerSpecModel, req.HandlerSpecReasoning, req.HandlerSpecDigest, m.Seq, stamp, stamp); err != nil {
		return zero, nil, err
	}
	need.AgentID = req.HandlerAgentID
	e.HandlerNeed = need
	return e, nil, nil
}

// handlerLimitWarning is set_limit's advice after a raise: the limit admits
// more teams than there are available database handlers.
func handlerLimitWarning(ctx context.Context, q queryRower, task string, limit int) (string, error) {
	free, err := freeQueueHandlers(ctx, q, task, nil)
	if err != nil {
		return "", err
	}
	available := len(free)
	if limit > 0 && limit <= available {
		return "", nil
	}
	noun := "handlers"
	if available == 1 {
		noun = "handler"
	}
	text := fmt.Sprintf("limit %d exceeds %d available database %s", limit, available, noun)
	if limit == 0 {
		text = fmt.Sprintf("no fixed limit with %d available database %s", available, noun)
	}
	on, err := queueHandlerProvision(ctx, q, task)
	if err != nil {
		return "", err
	}
	if on {
		return text + "; the runner adds one per waiting team while the agent cap allows", nil
	}
	return fmt.Sprintf("%s; automatic provisioning is off, so teams will wait. Fix: tt team queue provision --task %s --auto on", text, task), nil
}
