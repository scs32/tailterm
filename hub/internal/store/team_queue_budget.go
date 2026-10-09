package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The token budget of the team queue: the admission check that holds a queued
// entry while the remaining budget cannot cover its estimate plus the reserve,
// and the entry hold that keeps a running team from starting new turns once it
// has spent more than api.BudgetHoldMultiple times its item's saved estimate.
// Admission also reserves, for every team that still holds a slot, the part of
// its estimate the budget's source does not show as spent yet, so entries that
// each fit alone are not admitted together past what remains.
// Neither interrupts a turn, closes a session or refuses a write: the
// admission check only skips a queued entry at the head, and the hold is read
// by the relay before it wakes one of the team's runs.

func migrateTeamQueueBudget(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS team_queue_budget_holds(
 task_id TEXT NOT NULL,entry_id TEXT NOT NULL,item_id TEXT NOT NULL,estimate_tokens INTEGER NOT NULL,
 team_tokens TEXT NOT NULL,team_state TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('held','continued','stopped')),
 ask_agent TEXT NOT NULL DEFAULT '',ask_seq INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,resolved_at TEXT NOT NULL DEFAULT '',
 by_agent TEXT NOT NULL DEFAULT '',by_node TEXT NOT NULL DEFAULT '',by_user TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(task_id,entry_id,estimate_tokens));
 CREATE INDEX IF NOT EXISTS team_queue_budget_holds_state ON team_queue_budget_holds(task_id,state);`)
	return err
}

// queueGoRace reports whether a team's verification will include the Go race
// check, which decides between a lane's two default estimates. It follows the
// verification matrix: rule 2 of verification/matrix.json puts a path in the
// go group when it matches the prefix "hub/" (scripts/verify-matrix.mjs,
// selectChecks), and the go group always adds go-race. Queue ownership names
// files and directories, so a directory "hub" or under it counts too. An
// entry that declares no ownership (serial or unscoped) is verified against
// everything, so it is with Go race.
func queueGoRace(ownership []string) bool {
	if len(ownership) == 0 {
		return true
	}
	for _, path := range ownership {
		if path == "hub" || strings.HasPrefix(path, "hub/") {
			return true
		}
	}
	return false
}

// budgetTokenText prints a token figure for a reason: 44.00M, 12.50K or 800.
// A negative figure reads as 0.
func budgetTokenText(n *big.Rat) string {
	if n.Sign() < 0 {
		n = new(big.Rat)
	}
	switch {
	case n.Cmp(big.NewRat(1_000_000, 1)) >= 0:
		return new(big.Rat).Quo(n, big.NewRat(1_000_000, 1)).FloatString(2) + "M"
	case n.Cmp(big.NewRat(1_000, 1)) >= 0:
		return new(big.Rat).Quo(n, big.NewRat(1_000, 1)).FloatString(2) + "K"
	}
	return n.FloatString(0)
}

// queueBudgetCheck is the budget check of one claim or one listing. It reads
// the project's budget status once per host, its lane defaults once and the
// slot-holding entries with their teams' spend once, so a list of many queued
// entries costs one indexed window sum per budget row and host.
type queueBudgetCheck struct {
	task     string
	now      time.Time
	states   map[string][]usageBudgetState
	defaults *api.UsageEstimateDefaults
	// reserved is nil until loadReserved has run.
	reserved []budgetReservedEntry
	// outstanding caches reservedOutstanding by the instant the source
	// reflects, in Unix nanoseconds (zero is the allowance source), and by
	// whether failed entries were left out.
	outstanding map[budgetReservationKey]budgetReservation
}

type budgetReservationKey struct {
	upTo       int64
	skipFailed bool
}

// budgetReservedEntry is a slot-holding entry with the estimate it was
// admitted against and its team's runs.
type budgetReservedEntry struct {
	entry    api.TeamQueueEntry
	estimate int64
	runs     []api.BudgetHoldRun
}

// budgetReservation is what the slot-holding teams have not yet drawn from a
// budget row's source, and how many of them that is.
type budgetReservation struct {
	tokens *big.Rat
	teams  int
}

func newQueueBudgetCheck(task string, now time.Time) *queueBudgetCheck {
	return &queueBudgetCheck{task: task, now: now, states: map[string][]usageBudgetState{}, outstanding: map[budgetReservationKey]budgetReservation{}}
}

// loadReserved reads the project's entries that hold a slot (launching,
// running, or failed and not yet released: a failed team's runs can still
// spend), each with its estimate and its team. An entry with neither a saved
// estimate nor a lane default reserves nothing. Every result set is closed
// before the next query: a claim's transaction has one connection.
func (c *queueBudgetCheck) loadReserved(ctx context.Context, q queryRower) error {
	if c.reserved != nil {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND `+queueHoldsSQL+` ORDER BY position,id`, c.task)
	if err != nil {
		return err
	}
	entries := []api.TeamQueueEntry{}
	for rows.Next() {
		e, err := scanTeamQueue(rows)
		if err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	reserved := []budgetReservedEntry{}
	for _, e := range entries {
		estimate, _, err := c.entryEstimate(ctx, q, e)
		if err != nil {
			return err
		}
		if estimate == 0 {
			continue
		}
		runs, err := budgetHoldTeam(ctx, q, c.task, e.ID)
		if err != nil {
			return err
		}
		reserved = append(reserved, budgetReservedEntry{entry: e, estimate: estimate, runs: runs})
	}
	c.reserved = reserved
	return nil
}

// budgetTeamSpend sums what a team's runs have spent on their item, in turns
// at or before an instant when one is given. It reads usage_item_shares joined
// to usage_turn_totals for the turn's instant; a share whose turn has no
// totals row is not counted, which errs toward a larger reservation.
func budgetTeamSpend(ctx context.Context, q queryRower, task, item string, runs []api.BudgetHoldRun, upTo time.Time) (*big.Rat, error) {
	spent := new(big.Rat)
	for _, r := range runs {
		query := `SELECT s.denominator,sum(s.tokens),max(s.partial) FROM usage_item_shares s
 JOIN usage_turn_totals t ON t.task_id=s.task_id AND t.agent_id=s.agent_id AND t.run_id=s.run_id AND t.request_id=s.request_id
 WHERE s.task_id=? AND s.agent_id=? AND s.run_id=? AND s.item_task_id=? AND s.item_id=?`
		args := []any{task, r.AgentID, r.RunID, task, item}
		if !upTo.IsZero() {
			query += ` AND t.at_ns<=?`
			args = append(args, upTo.UnixNano())
		}
		rows, err := q.QueryContext(ctx, query+` GROUP BY s.denominator`, args...)
		if err != nil {
			return nil, err
		}
		if _, err = sumUsageItemShares(rows, spent, "not measured"); err != nil {
			return nil, err
		}
	}
	return spent, nil
}

// reservedOutstanding is, for one budget row's source, the sum over the
// slot-holding entries of max(0, estimate - team spend the source already
// reflects). The allowance source subtracts every reported turn, so all of a
// team's spend is reflected. A provider reading reflects use only up to its
// capture instant, so spend uploaded after it does not shrink the reservation
// until a newer reading arrives. With skipFailed the failed entries are left
// out: what would be reserved if every failed entry were released.
func (c *queueBudgetCheck) reservedOutstanding(ctx context.Context, q queryRower, state usageBudgetState, skipFailed bool) (budgetReservation, error) {
	var upTo time.Time
	if state.source == api.UsageBudgetSourceProvider {
		upTo = state.captured
	}
	key := budgetReservationKey{skipFailed: skipFailed}
	if !upTo.IsZero() {
		key.upTo = upTo.UnixNano()
	}
	if cached, ok := c.outstanding[key]; ok {
		return cached, nil
	}
	if err := c.loadReserved(ctx, q); err != nil {
		return budgetReservation{}, err
	}
	out := budgetReservation{tokens: new(big.Rat)}
	for _, r := range c.reserved {
		if skipFailed && r.entry.State == "failed" {
			continue
		}
		spent, err := budgetTeamSpend(ctx, q, c.task, r.entry.ItemID, r.runs, upTo)
		if err != nil {
			return budgetReservation{}, err
		}
		if left := new(big.Rat).Sub(big.NewRat(r.estimate, 1), spent); left.Sign() > 0 {
			out.tokens.Add(out.tokens, left)
			out.teams++
		}
	}
	c.outstanding[key] = out
	return out, nil
}

// entryEstimate is what admission compares with the budget for an entry: the
// item's saved estimate, else the project's lane default for the entry's
// template and Go race, returned with its label, else zero.
func (c *queueBudgetCheck) entryEstimate(ctx context.Context, q queryRower, e api.TeamQueueEntry) (int64, *api.TeamQueueEstimateDefault, error) {
	var saved int64
	err := q.QueryRowContext(ctx, `SELECT estimate_tokens FROM work_items WHERE task_id=? AND id=?`, c.task, e.ItemID).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, err
	}
	if saved > 0 {
		return saved, nil, nil
	}
	if c.defaults == nil {
		defaults, err := readUsageEstimateDefaults(ctx, q, c.task)
		if err != nil {
			return 0, nil, err
		}
		c.defaults = &defaults
	}
	if !c.defaults.Configured {
		return 0, nil, nil
	}
	label := &api.TeamQueueEstimateDefault{Lane: "planned", GoRace: queueGoRace(e.Ownership)}
	if e.Template == "small" {
		label.Lane = "small"
	}
	switch {
	case label.Lane == "small" && label.GoRace:
		label.Tokens = c.defaults.SmallRaceTokens
	case label.Lane == "small":
		label.Tokens = c.defaults.SmallTokens
	case label.GoRace:
		label.Tokens = c.defaults.PlannedRaceTokens
	default:
		label.Tokens = c.defaults.PlannedTokens
	}
	return label.Tokens, label, nil
}

// estimateDefaultText is the label of a lane default: "planned, Go race".
func estimateDefaultText(d api.TeamQueueEstimateDefault) string {
	if d.GoRace {
		return d.Lane + ", Go race"
	}
	return d.Lane
}

// admission returns why the budget holds a queued entry, or "" when it does
// not: the project has no budget row (the check is off), or every row has a
// usable source and the entry's estimate fits what remains after the reserve
// and what the slot-holding teams have not yet drawn from that source.
func (c *queueBudgetCheck) admission(ctx context.Context, q queryRower, e api.TeamQueueEntry) (string, error) {
	return c.admissionReason(ctx, q, e, false)
}

// heldOnlyByFailed reports whether a queued entry the budget holds would fit
// if every failed entry were released: it waits for a release, not for the
// budget, so its stall behind the failed entry must stay visible.
func (c *queueBudgetCheck) heldOnlyByFailed(ctx context.Context, q queryRower, e api.TeamQueueEntry) (bool, error) {
	if err := c.loadReserved(ctx, q); err != nil {
		return false, err
	}
	failed := false
	for _, r := range c.reserved {
		failed = failed || r.entry.State == "failed"
	}
	if !failed {
		return false, nil
	}
	reason, err := c.admissionReason(ctx, q, e, true)
	return reason == "", err
}

func (c *queueBudgetCheck) admissionReason(ctx context.Context, q queryRower, e api.TeamQueueEntry, skipFailed bool) (string, error) {
	states, loaded := c.states[e.Host]
	if !loaded {
		var err error
		if states, err = usageBudgetStatus(ctx, q, c.task, e.Host, c.now); err != nil {
			return "", err
		}
		c.states[e.Host] = states
	}
	if len(states) == 0 {
		return "", nil
	}
	estimate, label, err := c.entryEstimate(ctx, q, e)
	if err != nil {
		return "", err
	}
	if estimate == 0 {
		return "Token budget: no estimate and no lane default; set one with tt usage defaults set", nil
	}
	needs := budgetTokenText(big.NewRat(estimate, 1)) + " tokens"
	if label != nil {
		needs += " (default, " + estimateDefaultText(*label) + ")"
	}
	for _, state := range states {
		row := state.row
		name := fmt.Sprintf("Token budget (%s %s)", row.Runtime, row.Window)
		if state.source == api.UsageBudgetSourceNone {
			return fmt.Sprintf("%s: no usable source. Provider reading for host %s %s and no reset time is set", name, e.Host, state.reading), nil
		}
		reserve := big.NewRat(row.AllowanceTokens*int64(row.ReservePercent), 100)
		admitted, err := c.reservedOutstanding(ctx, q, state, skipFailed)
		if err != nil {
			return "", err
		}
		free := new(big.Rat).Sub(state.remaining, reserve)
		if free.Sub(free, admitted.tokens).Cmp(big.NewRat(estimate, 1)) >= 0 {
			continue
		}
		teams := ""
		if admitted.teams == 1 {
			teams = fmt.Sprintf(", %s reserved for 1 admitted team", budgetTokenText(admitted.tokens))
		} else if admitted.teams > 1 {
			teams = fmt.Sprintf(", %s reserved for %d admitted teams", budgetTokenText(admitted.tokens), admitted.teams)
		}
		source := "provider reading"
		if state.source == api.UsageBudgetSourceAllowance {
			source = fmt.Sprintf("allowance minus reported usage; provider reading for host %s %s", e.Host, state.reading)
		}
		return fmt.Sprintf("%s: needs about %s; %s remain before the reset at %s, reserve %d%%%s (source: %s)", name, needs, budgetTokenText(state.remaining), state.reset.UTC().Format(time.RFC3339), row.ReservePercent, teams, source), nil
	}
	return "", nil
}

// queueBudgetAdmission is the budget check for one queued entry: the reason
// it is held, or "".
func queueBudgetAdmission(ctx context.Context, q queryRower, task string, e api.TeamQueueEntry, now time.Time) (string, error) {
	return newQueueBudgetCheck(task, now).admission(ctx, q, e)
}

// budgetHoldCols lists the hold columns loadBudgetHold scans.
const budgetHoldCols = `entry_id,item_id,estimate_tokens,team_tokens,team_state,state,ask_agent,ask_seq,created_at,resolved_at,by_agent,by_node,by_user`

func scanBudgetHold(row interface{ Scan(...any) error }) (api.BudgetHold, error) {
	var h api.BudgetHold
	var by api.Sender
	if err := row.Scan(&h.EntryID, &h.ItemID, &h.EstimateTokens, &h.TeamTokens, &h.TeamState, &h.State, &h.AskAgent, &h.AskSeq, &h.CreatedAt, &h.ResolvedAt, &by.AgentID, &by.Node, &by.User); err != nil {
		return h, err
	}
	if h.ResolvedAt != "" {
		h.ResolvedBy = &by
	}
	return h, nil
}

// activeBudgetHold is the entry's hold in state held, if any.
func activeBudgetHold(ctx context.Context, q queryRower, task, entry string) (*api.BudgetHold, error) {
	h, err := scanBudgetHold(q.QueryRowContext(ctx, `SELECT `+budgetHoldCols+` FROM team_queue_budget_holds WHERE task_id=? AND entry_id=? AND state='held' ORDER BY created_at DESC,estimate_tokens DESC LIMIT 1`, task, entry))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// budgetHoldSubject is the constant subject of the ask a hold posts.
const budgetHoldSubject = "A team has passed three times its token estimate and is held"

// budgetHoldCommands are the two commands that end a hold, as the ask, the
// list and the docs give them. The stop is run from an unbound owner shell:
// the CLI refuses owner-side queue changes from a session with an agent
// identity.
func budgetHoldCommands(task, entry string) (string, string) {
	return fmt.Sprintf("tt usage hold continue --project %s --entry %s", task, entry),
		fmt.Sprintf("tt team queue fail --task %s --entry %s --reason TEXT", task, entry)
}

// holdEntryBudget records that a running entry's team passed the hold
// multiple of the item's saved estimate and asks the owner helper to continue
// or stop it. It does so once per entry and estimate value, and never while
// the entry already has a hold in force. It reports whether it wrote a hold.
// Its caller holds a savepoint, so a failure leaves nothing behind.
func (s *Store) holdEntryBudget(ctx context.Context, tx *sql.Tx, task api.Task, item, entry string, estimate int64, team *api.TeamTokenBudget, actual *big.Rat) (bool, error) {
	var seen int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_budget_holds WHERE task_id=? AND entry_id=? AND (estimate_tokens=? OR state='held')`, task.ID, entry, estimate).Scan(&seen)
	if err != nil || seen != 0 {
		return false, err
	}
	var title string
	if err = tx.QueryRowContext(ctx, `SELECT title FROM work_items WHERE task_id=? AND id=?`, task.ID, item).Scan(&title); err != nil {
		return false, err
	}
	bound := ""
	if team.ActualState == "partial" {
		bound = "at least "
	}
	continueCommand, stopCommand := budgetHoldCommands(task.ID, entry)
	ratio := new(big.Rat).Quo(actual, big.NewRat(estimate, 1))
	text := fmt.Sprintf("Item %s (%s): the team of queue entry %s has used %s%s tokens on it against a saved estimate of %d: %s times, past %d times. The relay starts no new turn for this team until you decide; a turn in progress and every write finish. To continue: %s (save a larger estimate first with tt work-items update --estimate-tokens if the team should be held again later). To stop, from an unbound owner shell: %s. Acknowledge this message with tt ack, act, then answer it.",
		item, stallNoticeDetail(title), entry, bound, actual.FloatString(0), estimate, ratio.FloatString(2), api.BudgetHoldMultiple, continueCommand, stopCommand)
	refs := map[string]string{"item": item, "entry": entry, "estimateTokens": strconv.FormatInt(estimate, 10), "teamTokens": team.ActualTokens, "teamState": team.ActualState,
		"ratio": ratio.RatString(), "multiple": strconv.Itoa(api.BudgetHoldMultiple), "continue": continueCommand, "stop": stopCommand}
	by := api.Caller{Node: "system", User: "usage-hold"}
	helper, ok, err := currentOwnerHelper(ctx, tx, task.ID)
	if err != nil {
		return false, err
	}
	var askAgent string
	var askSeq int64
	if ok {
		req := api.PostMessageRequest{To: helper.ID, RequestID: fmt.Sprintf("usage-hold-%s-%s-%d", item, entry, estimate), Envelope: &api.Envelope{
			Kind: api.EnvelopeKindQuestion, To: helper.Name, Subject: budgetHoldSubject, Refs: refs,
			Body: api.EnvelopeBody{Question: "Continue or stop the held team?", Options: map[string]string{"continue": "Lift the hold: " + continueCommand, "stop": "Fail the entry with a reason: " + stopCommand}, Text: text}}}
		if err = api.NormalizeEnvelopePost(&req); err != nil {
			return false, err
		}
		// Like a warning, the ask never resumes a retired owner helper.
		message, err := s.insertMessageWithResume(ctx, tx, task, req, helper, by, false, false, false)
		if err != nil {
			return false, err
		}
		if err = insertMessagePostReceipt(ctx, tx, &message, req.RequestID, requestHash(req), by); err != nil {
			return false, err
		}
		// A question obliges an answer, and its wake job reaches the helper.
		if err = s.createObligations(ctx, tx, message, req, false); err != nil {
			return false, err
		}
		askAgent, askSeq = helper.ID, message.Seq
	} else {
		// No owner helper: the Board carries the same figures to the owner.
		refs["escalation"] = "owner"
		notice := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: budgetHoldSubject, Refs: refs, Body: api.EnvelopeBody{Text: text}}
		message, err := s.insertMessage(ctx, tx, task, api.PostMessageRequest{Envelope: &notice}, api.Agent{}, by, false, false)
		if err != nil {
			return false, err
		}
		askSeq = message.Seq
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO team_queue_budget_holds(task_id,entry_id,item_id,estimate_tokens,team_tokens,team_state,state,ask_agent,ask_seq,created_at) VALUES(?,?,?,?,?,?,'held',?,?,?)`,
		task.ID, entry, item, estimate, team.ActualTokens, team.ActualState, askAgent, askSeq, ts(s.now()))
	return err == nil, err
}

// continueEntryBudget lifts an entry's hold. Only the owner (no agent
// identity) or the project's owner helper may. It writes no estimate: a newly
// saved one re-arms the hold at the new value, and without one this entry is
// never held again at this value.
func continueEntryBudget(ctx context.Context, tx *sql.Tx, task string, e api.TeamQueueEntry, req api.TeamQueueRequest, now string) error {
	if req.AgentID != "" {
		helper, ok, err := currentOwnerHelper(ctx, tx, task)
		if err != nil {
			return err
		}
		if !ok || helper.ID != req.AgentID {
			return fmt.Errorf("%w: only the owner or the owner helper continues a held team", api.ErrConflict)
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE team_queue_budget_holds SET state='continued',resolved_at=?,by_agent=?,by_node=?,by_user=? WHERE task_id=? AND entry_id=? AND state='held'`,
		now, req.AgentID, req.Caller.Node, req.Caller.User, task, e.ID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: entry %s has no token budget hold in force", api.ErrConflict, e.ID)
	}
	return nil
}

// stopEntryBudgetHold resolves an entry's hold in force as stopped, when its
// entry fails, finishes, is released or is recorded as integrated. An entry
// without one matches no row.
func stopEntryBudgetHold(ctx context.Context, tx *sql.Tx, task, entry, now string, by api.Caller) error {
	_, err := tx.ExecContext(ctx, `UPDATE team_queue_budget_holds SET state='stopped',resolved_at=?,by_node=?,by_user=? WHERE task_id=? AND entry_id=? AND state='held'`, now, by.Node, by.User, task, entry)
	return err
}

// stopItemBudgetHolds does the same for whichever entry of an item is running.
func stopItemBudgetHolds(ctx context.Context, tx *sql.Tx, task, item, now string) error {
	_, err := tx.ExecContext(ctx, `UPDATE team_queue_budget_holds SET state='stopped',resolved_at=?,by_node='team_close',by_user='lead' WHERE task_id=? AND item_id=? AND state='held'
 AND entry_id IN (SELECT id FROM team_queue_entries WHERE task_id=? AND item_id=? AND state='running' AND released_at='')`, now, task, item, task, item)
	return err
}

// budgetHoldTeam lists the runs a hold keeps from waking: the set
// loadEntryTokenActual counts. They are the runs bound to the entry's item
// whose agent has no project role (so never a database handler, backlog
// steward, deployment agent or owner helper) and whose binding is not older
// than the entry's admission, each with its exact run id.
func budgetHoldTeam(ctx context.Context, q queryRower, task, entry string) ([]api.BudgetHoldRun, error) {
	var item, created, claimed string
	err := q.QueryRowContext(ctx, `SELECT item_id,created_at,admitted_at FROM team_queue_entries WHERE task_id=? AND id=?`, task, entry).Scan(&item, &created, &claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	admitted := parseTS(claimed)
	if admitted.IsZero() {
		admitted = parseTS(created)
	}
	if admitted.IsZero() {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT b.agent_id,b.run_id,b.created_at FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id
 WHERE b.item_task_id=? AND b.item_id=? AND a.role='' ORDER BY b.agent_id,b.run_id`, task, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []api.BudgetHoldRun{}
	for rows.Next() {
		var run api.BudgetHoldRun
		var bound string
		if err := rows.Scan(&run.AgentID, &run.RunID, &bound); err != nil {
			return nil, err
		}
		if at := parseTS(bound); !at.IsZero() && !at.Before(admitted) {
			runs = append(runs, run)
		}
	}
	return runs, rows.Err()
}

// budgetHoldsSQL selects the holds that keep runs from waking: a hold in
// force, and a stopped one whose entry is failed and not yet released, so a
// stopped team starts no new turn while its sessions are cleaned up.
const budgetHoldsSQL = `SELECT h.entry_id,h.item_id,h.estimate_tokens,h.team_tokens,h.team_state,h.state,h.ask_agent,h.ask_seq,h.created_at,h.resolved_at,h.by_agent,h.by_node,h.by_user
 FROM team_queue_budget_holds h JOIN team_queue_entries e ON e.task_id=h.task_id AND e.id=h.entry_id
 WHERE h.task_id=? AND (h.state='held' OR (h.state='stopped' AND e.state='failed' AND e.released_at=''))`

// budgetHeldRun reports whether an exact run belongs to a team that a hold
// keeps from waking: the one check behind the wake-job lease guard and the
// holds read the relay makes before an inbox delivery. It is one read by the
// run's binding. The run must be bound to the held entry's item at or after
// the entry's admission, and its agent must have no project role.
func budgetHeldRun(ctx context.Context, q queryRower, task, agent, run string) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT b.created_at,e.created_at,e.admitted_at FROM agent_work_item_bindings b
 JOIN agents a ON a.id=b.agent_id
 JOIN team_queue_budget_holds h ON h.task_id=b.item_task_id AND h.item_id=b.item_id
 JOIN team_queue_entries e ON e.task_id=h.task_id AND e.id=h.entry_id
 WHERE b.agent_id=? AND b.run_id=? AND b.item_task_id=? AND a.role='' AND (h.state='held' OR (h.state='stopped' AND e.state='failed' AND e.released_at=''))`, agent, run, task)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var bound, created, claimed string
		if err := rows.Scan(&bound, &created, &claimed); err != nil {
			return false, err
		}
		admitted := parseTS(claimed)
		if admitted.IsZero() {
			admitted = parseTS(created)
		}
		if at := parseTS(bound); !admitted.IsZero() && !at.IsZero() && !at.Before(admitted) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// BudgetHolds reads the project's holds that keep runs from waking, each with
// its team's runs. With an agent and run it answers only whether that exact
// run is held, with its hold: the relay asks this before every wake.
func (s *Store) BudgetHolds(ctx context.Context, task, agent, run string) (api.UsageHolds, error) {
	out := api.UsageHolds{Holds: []api.BudgetHold{}}
	exact := agent != "" || run != ""
	if !api.ValidID(task, "tsk") || (exact && (!api.ValidID(agent, "agt") || !validRunID(run))) {
		return out, api.ErrInvalid
	}
	if err := usageProjectExists(ctx, s.db, task); err != nil {
		return out, err
	}
	query, args := budgetHoldsSQL+` ORDER BY h.created_at,h.entry_id`, []any{task}
	if exact {
		// The same check the wake-job lease guard makes.
		held, err := budgetHeldRun(ctx, s.db, task, agent, run)
		if err != nil || !held {
			return out, err
		}
		// Only a hold on the entry of the item this run is bound to.
		query = budgetHoldsSQL + ` AND h.item_id IN (SELECT b.item_id FROM agent_work_item_bindings b WHERE b.agent_id=? AND b.run_id=? AND b.item_task_id=?) ORDER BY h.created_at,h.entry_id`
		args = append(args, agent, run, task)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	var holds []api.BudgetHold
	for rows.Next() {
		h, err := scanBudgetHold(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		holds = append(holds, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, h := range holds {
		// An entry held again at a new estimate after a stop lists once.
		if seen[h.EntryID] {
			continue
		}
		if h.Runs, err = budgetHoldTeam(ctx, s.db, task, h.EntryID); err != nil {
			return out, err
		}
		if !exact {
			seen[h.EntryID] = true
			out.Holds = append(out.Holds, h)
			continue
		}
		for _, r := range h.Runs {
			if r.AgentID == agent && r.RunID == run {
				seen[h.EntryID] = true
				h.Runs = []api.BudgetHoldRun{r}
				out.Held, out.Holds = true, append(out.Holds, h)
				break
			}
		}
	}
	out.Held = out.Held || (!exact && len(out.Holds) > 0)
	return out, nil
}

// budgetHeldReason is the block reason of a running entry with a hold in force.
func budgetHeldReason(task string, h api.BudgetHold) string {
	continueCommand, stopCommand := budgetHoldCommands(task, h.EntryID)
	tokens, _ := new(big.Rat).SetString(h.TeamTokens)
	if tokens == nil {
		tokens = new(big.Rat)
	}
	return fmt.Sprintf("Held past %d times its token estimate (%s used, estimate %s); no new turns start. The owner helper continues it: %s, or stops it from an unbound owner shell: %s",
		api.BudgetHoldMultiple, budgetTokenText(tokens), budgetTokenText(big.NewRat(h.EstimateTokens, 1)), continueCommand, stopCommand)
}

// explainQueueBudget fills what a listing shows of the budget: the lane
// default of an entry whose item has no saved estimate, the hold of a running
// entry, and the reason the budget holds a queued entry, which it returns by
// entry id. A reason is written only where nothing earlier explained the wait.
// An entry that only the reservation of a failed, unreleased entry holds is
// not returned: its wait ends with a release, so its stall stays.
func explainQueueBudget(ctx context.Context, q queryRower, task string, out *api.TeamQueueList, now time.Time) (map[string]string, error) {
	check := newQueueBudgetCheck(task, now)
	held := map[string]string{}
	for i := range out.Entries {
		e := &out.Entries[i]
		if e.TaskID != task || (e.State != "queued" && e.State != "launching" && e.State != "running") {
			continue
		}
		_, label, err := check.entryEstimate(ctx, q, *e)
		if err != nil {
			return nil, err
		}
		e.EstimateDefault = label
		if e.State == "running" {
			hold, err := activeBudgetHold(ctx, q, task, e.ID)
			if err != nil {
				return nil, err
			}
			if hold != nil {
				e.BudgetHold = hold
				e.BlockReason = budgetHeldReason(task, *hold)
			}
		}
		if e.State != "queued" {
			continue
		}
		reason, err := check.admission(ctx, q, *e)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			if e.BlockReason == "" {
				e.BlockReason = reason
			}
			release, err := check.heldOnlyByFailed(ctx, q, *e)
			if err != nil {
				return nil, err
			}
			if !release {
				held[e.ID] = reason
			}
		}
	}
	return held, nil
}

// suppressBudgetStalls clears the stalls the budget explains. A queued entry
// the budget holds is waiting in the ordinary way, like one waiting for host
// capacity, so it has no stall and shows its budget reason. A queued entry
// stalled behind a running entry with a hold in force waits for the owner
// helper's decision, which the hold's own ask already carries. The stall
// notice recomputes the list and refuses an entry with no stall, so clearing
// it here also stops the notice.
func suppressBudgetStalls(out *api.TeamQueueList, held map[string]string) {
	holds := map[string]bool{}
	for _, e := range out.Entries {
		if e.BudgetHold != nil {
			holds[e.ID] = true
		}
	}
	for i := range out.Entries {
		e := &out.Entries[i]
		if e.State != "queued" || e.Stall == nil {
			continue
		}
		if reason := held[e.ID]; reason != "" {
			e.Stall, e.BlockReason = nil, reason
			continue
		}
		if blocker := e.Stall.BlockerEntryID; holds[blocker] {
			e.Stall = nil
			e.BlockReason = fmt.Sprintf("Waits behind entry %s, held past %d times its token estimate until the owner helper continues or stops it", blocker, api.BudgetHoldMultiple)
		}
	}
}
