package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/scs32/tailterm/hub/internal/api"
)

const reviewConvergenceSchema = `CREATE TABLE IF NOT EXISTS review_convergence (
 task_id TEXT NOT NULL, item_id TEXT NOT NULL, state_json TEXT NOT NULL,
 PRIMARY KEY(task_id,item_id));`

func reviewConflict(reason string) error {
	return fmt.Errorf("%w: review convergence: %s", api.ErrConflict, reason)
}
func reviewState(ctx context.Context, q queryRower, task, item string) (api.ReviewConvergence, error) {
	out := api.ReviewConvergence{ItemID: item, History: "unknown", Scopes: []api.ReviewScope{}, Rounds: []api.ReviewRound{}, FollowUps: []api.ReviewFollowUp{}, Focused: []api.FocusedReview{}}
	var raw string
	err := q.QueryRowContext(ctx, `SELECT state_json FROM review_convergence WHERE task_id=? AND item_id=?`, task, item).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(raw), &out)
	return out, err
}
func saveReviewState(ctx context.Context, tx *sql.Tx, task string, state api.ReviewConvergence) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_convergence(task_id,item_id,state_json) VALUES(?,?,?) ON CONFLICT(task_id,item_id) DO UPDATE SET state_json=excluded.state_json`, task, state.ItemID, string(raw))
	return err
}

// A review stage is the run of scope revisions worked under one owner order.
// A scope revision opens a new stage when its saved scope confirmation names a
// different owner order than the stage before it. Earlier stages stay stored
// unchanged; the two-round count, verdicts, blockers, focused records and
// follow-up IDs are evaluated inside the current stage only.
type earlierReviewStages struct {
	rounds    []api.ReviewRound
	focused   []api.FocusedReview
	followUps []api.ReviewFollowUp
}

// reviewStageStart returns the assignment sequence that opens the current
// stage, or 0 in the first stage. Only an assignment opens a stage: assign is
// the sequence of an ASSIGN being applied to scope revision current, else 0, so
// an open review of the current stage still takes its result after a new owner
// order is confirmed. Unknown legacy history never opens a stage. A scope with
// no saved confirmation, or one whose rounds continue an earlier scope's
// numbering, stays in its stage.
func reviewStageStart(ctx context.Context, q queryRower, task string, state api.ReviewConvergence, current, assign int64) (int64, error) {
	scopes := state.Scopes
	if assign != 0 && scopeFor(&state, current) == nil {
		scopes = append(scopes[:len(scopes):len(scopes)], api.ReviewScope{ScopeRevision: current, AssignmentSeq: assign})
	}
	if state.History == "unknown" || len(scopes) < 2 {
		return 0, nil
	}
	var start, order int64
	for _, sc := range scopes {
		var confirmed int64
		err := q.QueryRowContext(ctx, `SELECT order_seq FROM work_order_scope_confirmations WHERE task_id=? AND item_id=? AND scope_revision=? ORDER BY item_revision,order_seq LIMIT 1`, task, state.ItemID, sc.ScopeRevision).Scan(&confirmed)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if order == 0 {
			order = confirmed // the first known order names the first stage
		}
		if confirmed == order {
			continue
		}
		continued := false
		for _, r := range state.Rounds {
			if r.RequestSeq >= sc.AssignmentSeq {
				continued = r.Number != 1
				break
			}
		}
		if !continued {
			start, order = sc.AssignmentSeq, confirmed
		}
	}
	return start, nil
}

// currentReviewStage narrows state to its current stage and returns the
// earlier stages' records for saveReviewStage.
func currentReviewStage(ctx context.Context, q queryRower, task string, state *api.ReviewConvergence, current, assign int64) (earlierReviewStages, error) {
	var earlier earlierReviewStages
	start, err := reviewStageStart(ctx, q, task, *state, current, assign)
	if err != nil || start == 0 {
		return earlier, err
	}
	n := 0
	for n < len(state.Rounds) && state.Rounds[n].RequestSeq < start {
		n++
	}
	earlier.rounds, state.Rounds = state.Rounds[:n:n], state.Rounds[n:]
	n = 0
	for n < len(state.Focused) && state.Focused[n].RequestSeq < start {
		n++
	}
	earlier.focused, state.Focused = state.Focused[:n:n], state.Focused[n:]
	n = 0
	for n < len(state.FollowUps) && state.FollowUps[n].MessageSeq < start {
		n++
	}
	earlier.followUps, state.FollowUps = state.FollowUps[:n:n], state.FollowUps[n:]
	return earlier, nil
}

// saveReviewStage saves the current stage after the unchanged earlier stages.
func saveReviewStage(ctx context.Context, tx *sql.Tx, task string, earlier earlierReviewStages, state api.ReviewConvergence) error {
	if len(earlier.rounds) > 0 {
		state.Rounds = append(earlier.rounds, state.Rounds...)
	}
	if len(earlier.focused) > 0 {
		state.Focused = append(earlier.focused, state.Focused...)
	}
	if len(earlier.followUps) > 0 {
		state.FollowUps = append(earlier.followUps, state.FollowUps...)
	}
	return saveReviewState(ctx, tx, task, state)
}
func (s *Store) ListReviewConvergence(ctx context.Context, task string) ([]api.ReviewConvergence, error) {
	if !api.ValidID(task, "tsk") {
		return nil, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM work_items WHERE task_id=? ORDER BY seq`, task)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []api.ReviewConvergence{}
	for _, id := range ids {
		v, e := reviewState(ctx, tx, task, id)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, tx.Commit()
}
func reviewLead(ctx context.Context, tx *sql.Tx, task, item, agent, run string) error {
	// A human owner may record a lead disposition. Agent authors must be exact leads.
	if agent == "" {
		return nil
	}
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND task_id=?`, agent, task))
	if err != nil || a.RunID != run || a.Status == api.AgentClosed || a.Status == api.AgentExited || a.Status == api.AgentRetired {
		return reviewConflict("current lead run required")
	}
	var lead string
	if err = tx.QueryRowContext(ctx, `SELECT orchestrator FROM tasks WHERE id=?`, task).Scan(&lead); err != nil {
		return err
	}
	scoped, err := scopedLeadItem(ctx, tx, task, agent, run)
	if err != nil {
		return err
	}
	if (scoped != "" && scoped != item) || (scoped == "" && lead != a.Name) {
		return reviewConflict("only lead may start review or record disposition")
	}
	return nil
}
func numberedCriteria(criteria map[string]string) bool {
	if len(criteria) == 0 {
		return false
	}
	for i := 1; i <= len(criteria); i++ {
		if strings.TrimSpace(criteria["a"+strconv.Itoa(i)]) == "" {
			return false
		}
	}
	return true
}
func normalizedVerificationCriteria(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}
func verificationOwned(ids []string, key string) bool {
	for _, id := range ids {
		if id == key {
			return true
		}
	}
	return false
}
func compatibleVerificationDesignation(state api.ReviewConvergence, ids []string) error {
	for _, id := range ids {
		for _, round := range state.Rounds {
			if round.ResultSeq != 0 && round.Verdicts[id] != "" && round.Verdicts[id] != "pending-verification" {
				return reviewConflict("cannot reclassify completed reviewer verdict for " + id)
			}
			for _, blocker := range round.Blockers {
				if blocker.Criterion == id {
					return reviewConflict("cannot reclassify criterion with prior blocker " + id)
				}
			}
		}
	}
	return nil
}
func completeVerdicts(criteria map[string]string, owned []string, verdicts map[string]string) error {
	if len(criteria) != len(verdicts) {
		return reviewConflict("verdicts must cover every frozen criterion")
	}
	for key := range criteria {
		v := verdicts[key]
		if verificationOwned(owned, key) {
			if v != "pending-verification" {
				return reviewConflict("verification-owned criterion " + key + " must be pending-verification")
			}
			continue
		}
		if v != "pass" && v != "fail" && v != "partial" {
			return reviewConflict("invalid or missing verdict for " + key)
		}
	}
	return nil
}
func evidenceFinding(f api.ReviewFinding, candidate string, criteria map[string]string, blocker bool) error {
	if f.ID == "" || strings.TrimSpace(f.Title) == "" {
		return reviewConflict("finding needs stable ID and title")
	}
	if f.Criterion != "" && criteria[f.Criterion] == "" {
		return reviewConflict("unknown criterion")
	}
	if !((strings.TrimSpace(f.Command) != "" && strings.TrimSpace(f.Output) != "") || (strings.TrimSpace(f.File) != "" && f.Line > 0)) {
		return reviewConflict("finding requires command and output or file and line")
	}
	if blocker && f.Criterion == "" && !f.Regression {
		return reviewConflict("blocker must cite failed criterion or regression")
	}
	if f.Regression && (!validGitCommit(f.Baseline) || f.Candidate != candidate || f.Baseline == candidate) {
		return reviewConflict("regression requires distinct exact baseline and candidate")
	}
	return nil
}
func scopeFor(state *api.ReviewConvergence, scope int64) *api.ReviewScope {
	for i := range state.Scopes {
		if state.Scopes[i].ScopeRevision == scope {
			return &state.Scopes[i]
		}
	}
	return nil
}
func outstanding(state api.ReviewConvergence) map[string]api.ReviewFinding {
	out := map[string]api.ReviewFinding{}
	for _, r := range state.Rounds {
		if r.ResultSeq != 0 {
			for _, f := range r.Blockers {
				out[f.ID] = f
			}
			if r.Number == 2 {
				for _, prior := range state.Rounds[0].Blockers {
					found := false
					for _, f := range r.Blockers {
						if f.ID == prior.ID {
							found = true
						}
					}
					if !found && r.Verdicts[prior.Criterion] == "pass" && !prior.Regression {
						delete(out, prior.ID)
					}
				}
			}
		}
	}
	// Demotion changes filing, not the failed criterion's verification requirement.
	if len(state.Rounds) > 0 {
		last := state.Rounds[len(state.Rounds)-1]
		findings := last.Findings
		// Compatibility with the first ledger version, where findings were only
		// stored in the follow-up projection.
		if len(findings) == 0 {
			for _, f := range state.FollowUps {
				if f.MessageSeq == last.ResultSeq {
					findings = append(findings, f.Finding)
				}
			}
		}
		for _, f := range findings {
			if f.Criterion != "" && last.Verdicts[f.Criterion] != "pass" {
				out[f.ID] = f
			}
		}
	}
	for _, f := range state.Focused {
		if f.Passed {
			for _, id := range f.BlockerIDs {
				delete(out, id)
			}
		}
	}
	return out
}
func reviewReady(state api.ReviewConvergence, scope int64, candidate string) error {
	return reviewReadySettling(state, scope, candidate, nil)
}

// reviewReadySettling also treats the named round-two partial criteria as
// settled. A focused request uses it to ask whether its own passing result
// would leave the reviewed candidate converged.
func reviewReadySettling(state api.ReviewConvergence, scope int64, candidate string, settling []string) error {
	if state.History == "unknown" {
		if len(state.Scopes) > 0 {
			return reviewConflict("legacy review history is unknown; do not infer a fresh lifetime count")
		}
		return nil
	} // preserve unassigned legacy state, explicitly unknown
	sc := scopeFor(&state, scope)
	if sc == nil {
		return reviewConflict("current scope is not assigned")
	}
	if len(state.Rounds) == 0 {
		return reviewConflict("no completed general review")
	}
	last := state.Rounds[len(state.Rounds)-1]
	if last.ResultSeq == 0 || !reflect.DeepEqual(roundCriteria(&state, last), sc.Criteria) || !reflect.DeepEqual(normalizedVerificationCriteria(last.VerificationCriteria), normalizedVerificationCriteria(sc.VerificationCriteria)) {
		return reviewConflict("latest general review is incomplete or stale")
	}
	verdicts := map[string]string{}
	for k, v := range last.Verdicts {
		verdicts[k] = v
	}
	fixable := outstandingWithoutFocused(state)
	for _, f := range state.Focused {
		if f.Passed && f.Candidate == candidate {
			for _, id := range f.BlockerIDs {
				if scopeResolved(f, id) {
					continue
				}
				if b, ok := fixable[id]; ok && b.Criterion != "" {
					verdicts[b.Criterion] = "pass"
				}
			}
			for _, id := range f.BlockerIDs {
				if scopeResolved(f, id) {
					continue
				}
				for _, r := range state.Rounds {
					for _, b := range r.Blockers {
						if b.ID == id && b.Criterion != "" {
							verdicts[b.Criterion] = "pass"
						}
					}
				}
			}
		}
	}
	// A focused record settles a partial in this projection only; the round's
	// own verdict is never rewritten. The latest verdict on the candidate decides.
	settled := map[string]bool{}
	for _, f := range state.Focused {
		if f.ResultSeq != 0 && f.Candidate == candidate {
			for _, id := range f.CriterionIDs {
				settled[id] = f.Passed
			}
		}
	}
	for _, id := range settling {
		settled[id] = true
	}
	for id, passed := range settled {
		if passed && verdicts[id] == "partial" {
			verdicts[id] = "pass"
		}
	}
	for key := range sc.Criteria {
		if verificationOwned(sc.VerificationCriteria, key) {
			if verdicts[key] != "pending-verification" {
				return reviewConflict("verification-owned criterion " + key + " was not pending")
			}
			continue
		}
		if verdicts[key] == "partial" {
			return reviewConflict("criterion " + key + " has not passed; a partial with no blocker is settled by focused verification naming it in criterionIds")
		}
		if verdicts[key] != "pass" {
			return reviewConflict("criterion " + key + " has not passed")
		}
	}
	if len(outstandingOnCandidate(state, candidate)) != 0 {
		return reviewConflict("unresolved blockers on accepted candidate")
	}
	if candidate != last.Candidate {
		changed := reviewConflict("changed candidate needs exact focused verification: of its blockerIds, of an identical-tree rebase, or of a lead-named fix with treeDiffers")
		if len(state.Focused) == 0 {
			return changed
		}
		f := state.Focused[len(state.Focused)-1]
		if !f.Passed || f.Candidate != candidate {
			return changed
		}
	}
	return nil
}

// receiptGated reports whether accepting the candidate relies on a settled
// partial or a lead-named fix. Those routes always need the exact receipt.
func receiptGated(state api.ReviewConvergence, candidate string) bool {
	for _, f := range state.Focused {
		if f.Passed && f.Candidate == candidate && (f.TreeDiffers || len(f.CriterionIDs) > 0) {
			return true
		}
	}
	return false
}

// exactReceipt returns the generation of the passing receipt for the exact
// candidate. Unlike verificationReady it also applies to an item that is not
// enrolled and has no verification-owned criterion.
func exactReceipt(ctx context.Context, tx *sql.Tx, item api.WorkItem, candidate string) (int64, error) {
	missing := reviewConflict("passing receipt for the current scope and exact candidate " + candidate + " required")
	done, err := checkVerificationCompletion(ctx, tx, item, candidate)
	if errors.Is(err, api.ErrConflict) {
		return 0, fmt.Errorf("%w (%s)", missing, strings.TrimPrefix(err.Error(), api.ErrConflict.Error()+": "))
	}
	if err != nil {
		return 0, err
	}
	if done != nil {
		return done.ReceiptGeneration, nil
	}
	records, err := verificationRecords(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return 0, err
	}
	p, r := currentVerification(records)
	if p == nil || r == nil || p.ScopeRevision != item.ScopeRevision || candidate == "" || p.Commit != candidate {
		return 0, missing
	}
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return 0, err
	}
	if sc := scopeFor(&state, item.ScopeRevision); sc == nil || sc.AssignmentSeq != p.AssignmentSeq {
		return 0, verificationConflict("assignment changed")
	}
	for _, round := range state.Rounds {
		if round.ReviewerID == r.VerifierAgentID {
			return 0, verificationConflict("reviewer cannot verify")
		}
	}
	if err = verificationEligible(*p, *r); err != nil {
		return 0, err
	}
	if _, err = validateVerificationKnownFailures(ctx, tx, *p, item, r); err != nil {
		return 0, err
	}
	return currentReceiptGeneration(records), nil
}
func reviewCompletion(ctx context.Context, tx *sql.Tx, item api.WorkItem, candidate string) error {
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	if _, err = currentReviewStage(ctx, tx, item.TaskID, &state, item.ScopeRevision, 0); err != nil {
		return err
	}
	accepted := candidate
	if accepted == "" && state.Disposition != nil {
		accepted = state.Disposition.Candidate
	}
	if err = verificationReady(ctx, tx, item, accepted); err != nil {
		return err
	}
	if state.History == "unknown" {
		if len(state.Scopes) > 0 {
			return reviewConflict("legacy review history is unknown")
		}
		return nil
	}
	if state.Disposition == nil || (state.Disposition.Kind != "accept" && state.Disposition.Kind != "owner-accept") {
		return reviewConflict("saved lead acceptance required")
	}
	if candidate != "" && candidate != state.Disposition.Candidate {
		return reviewConflict("acceptance candidate mismatch")
	}
	if state.Disposition.Kind == "owner-accept" {
		return nil // the owner explicitly accepted this exact candidate
	}
	if err = reviewReady(state, item.ScopeRevision, state.Disposition.Candidate); err != nil {
		return err
	}
	if receiptGated(state, state.Disposition.Candidate) {
		_, err = exactReceipt(ctx, tx, item, state.Disposition.Candidate)
	}
	return err
}

// applyReviewConvergence runs after immutable receipt replay and message insertion,
// before obligation outcomes in the SAME transaction. Any refusal rolls back all writes.
func (s *Store) applyReviewConvergence(ctx context.Context, tx *sql.Tx, m api.Message, req api.PostMessageRequest, target api.Agent, by api.Caller) error {
	e := req.Envelope
	if e == nil {
		parsed, matched := api.ParseTextConvention(req.Text)
		if matched && (parsed.Kind == "review" || parsed.Kind == "assign") {
			bound := 0
			if req.AgentID != "" {
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, req.AgentID, req.RunID).Scan(&bound); err != nil {
					return err
				}
			}
			if len(req.WorkItems) > 0 || bound > 0 {
				return reviewConflict("item review and assignment transitions require typed envelopes; text convention cannot bypass the ledger")
			}
		}
		return nil
	}
	var link *api.MessageWorkItem
	for i := range req.WorkItems {
		if req.WorkItems[i].Relationship == "primary" {
			link = &req.WorkItems[i]
		}
	}
	if link == nil {
		if req.AgentID != "" && (e.Kind == "review" || e.Kind == "result") {
			var bound int
			err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, req.AgentID, req.RunID).Scan(&bound)
			if err != nil {
				return err
			}
			if bound > 0 {
				return reviewConflict("item-bound reviews and results require native item links")
			}
		}
		if e.Review != nil {
			return reviewConflict("review transition needs native primary item link")
		}
		return nil
	}
	if link.ItemTaskID != m.TaskID {
		if e.Review != nil || e.Kind == "review" {
			return reviewConflict("review is item-project scoped")
		}
		return nil
	}
	item, err := getWorkItem(tx, ctx, m.TaskID, link.ItemID)
	if err != nil {
		return err
	}
	if link.ItemRevision != item.Revision && (e.Kind == "assign" || e.Kind == "review" || e.Review != nil) {
		return reviewConflict("current item revision required")
	}
	state, err := reviewState(ctx, tx, m.TaskID, item.ID)
	if err != nil {
		return err
	}
	var assigning int64
	if e.Kind == "assign" {
		assigning = m.Seq
	}
	earlier, err := currentReviewStage(ctx, tx, m.TaskID, &state, item.ScopeRevision, assigning)
	if err != nil {
		return err
	}
	meta := e.Review
	if meta != nil && req.AgentID != "" {
		var current, status string
		if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, req.AgentID, m.TaskID).Scan(&current, &status); err != nil {
			return err
		}
		if current != req.RunID || status == api.AgentClosed || status == api.AgentExited || status == api.AgentRetired {
			return reviewConflict("current sender run required")
		}
	}
	if e.Kind == "assign" {
		if err = reviewLead(ctx, tx, m.TaskID, item.ID, req.AgentID, req.RunID); err != nil {
			return err
		}
		if !numberedCriteria(e.Body.Acceptance) {
			return reviewConflict("assignment criteria must be contiguous a1..aN")
		}
		sc := scopeFor(&state, item.ScopeRevision)
		owned := normalizedVerificationCriteria(e.Body.VerificationCriteria)
		if err = compatibleVerificationDesignation(state, owned); err != nil {
			return err
		}
		if sc != nil && (!reflect.DeepEqual(sc.Criteria, e.Body.Acceptance) || !reflect.DeepEqual(normalizedVerificationCriteria(sc.VerificationCriteria), owned)) {
			return reviewConflict("criteria frozen; revision-checked scope update required")
		}
		if sc == nil {
			state.Scopes = append(state.Scopes, api.ReviewScope{ScopeRevision: item.ScopeRevision, ItemRevision: item.Revision, AssignmentSeq: m.Seq, Criteria: e.Body.Acceptance, VerificationCriteria: owned})
		}
		if len(state.Scopes) == 1 && state.Scopes[0].AssignmentSeq == m.Seq {
			var legacyReviews int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN message_work_item_links l ON l.message_seq=m.seq WHERE l.item_task_id=? AND l.item_id=? AND m.seq<? AND json_valid(m.envelope) AND json_extract(m.envelope,'$.kind')='review'`, item.TaskID, item.ID, m.Seq).Scan(&legacyReviews); err != nil {
				return err
			}
			if legacyReviews == 0 {
				state.History = "recorded"
			}
		}
		state.Disposition = nil
		return saveReviewStage(ctx, tx, m.TaskID, earlier, state)
	}
	if e.Kind == "review" {
		if err = reviewLead(ctx, tx, m.TaskID, item.ID, req.AgentID, req.RunID); err != nil {
			return err
		}
		if state.History == "unknown" {
			return reviewConflict("legacy review history is unknown; cannot allocate a fresh lifetime count")
		}
		if len(state.Rounds) >= 2 {
			return reviewConflict("third general review refused; use disposition or exact focused verification")
		}
		if len(state.Rounds) > 0 && state.Rounds[len(state.Rounds)-1].ResultSeq == 0 {
			return reviewConflict("previous general review is incomplete")
		}
		sc := scopeFor(&state, item.ScopeRevision)
		if err = compatibleVerificationDesignation(state, normalizedVerificationCriteria(e.Body.VerificationCriteria)); err != nil {
			return err
		}
		if sc == nil || !reflect.DeepEqual(sc.Criteria, e.Body.Acceptance) || !reflect.DeepEqual(normalizedVerificationCriteria(sc.VerificationCriteria), normalizedVerificationCriteria(e.Body.VerificationCriteria)) {
			return reviewConflict("review must use frozen assignment criteria")
		}
		if !validGitCommit(e.Body.Candidate) || target.ID == "" || target.RunID == "" {
			return reviewConflict("frozen candidate and exact reviewer run required")
		}
		if meta != nil && (meta.Mode != "general" || (meta.Candidate != "" && meta.Candidate != e.Body.Candidate)) {
			return reviewConflict("REVIEW is always a general review")
		}
		state.Rounds = append(state.Rounds, api.ReviewRound{Number: len(state.Rounds) + 1, ScopeRevision: item.ScopeRevision, Criteria: sc.Criteria, VerificationCriteria: normalizedVerificationCriteria(sc.VerificationCriteria), RequestSeq: m.Seq, Candidate: e.Body.Candidate, ReviewerID: target.ID, ReviewerRun: target.RunID, StartedAt: ts(s.now())})
		state.Disposition = nil
		return saveReviewStage(ctx, tx, m.TaskID, earlier, state)
	}
	if meta == nil {
		if e.Kind == "result" {
			for _, r := range state.Rounds {
				if reviewRequestSeq(r.RequestSeq, r.ActiveRequestSeq) == req.ReplyTo {
					return reviewConflict("review result requires structured review metadata")
				}
			}
			for _, f := range state.Focused {
				if reviewRequestSeq(f.RequestSeq, f.ActiveRequestSeq) == req.ReplyTo {
					return reviewConflict("focused result requires structured review metadata")
				}
			}
		}
		return nil
	}
	if meta.Mode == "reconcile" {
		return s.reconcileLegacyReviews(ctx, tx, m, req, item, &state)
	}
	if meta.Mode == "disposition" {
		if e.Kind != "notice" {
			return reviewConflict("disposition requires NOTICE")
		}
		// owner-accept is the owner's own resolution of an owner-decision or
		// follow-ups disposition: it binds one exact candidate and never comes
		// from an agent. Resolving follow-ups also requires the candidate's
		// independent verification to be ready, as an ordinary accept does.
		ownerAccept := meta.Disposition == "owner-accept"
		if ownerAccept {
			if req.AgentID != "" || req.RunID != "" {
				return reviewConflict("owner-accept requires an owner-authored notice")
			}
		} else if err = reviewLead(ctx, tx, m.TaskID, item.ID, req.AgentID, req.RunID); err != nil {
			return err
		}
		if meta.Disposition != "accept" && meta.Disposition != "owner-decision" && meta.Disposition != "follow-ups" && !ownerAccept {
			return reviewConflict("unknown disposition")
		}
		if len(state.Rounds) == 0 || state.Rounds[len(state.Rounds)-1].ResultSeq == 0 {
			return reviewConflict("completed review required for disposition")
		}
		if ownerAccept && (state.Disposition == nil || (state.Disposition.Kind != "owner-decision" && state.Disposition.Kind != "follow-ups")) {
			return reviewConflict("owner-accept resolves an owner-decision or follow-ups disposition")
		}
		if ownerAccept && state.Disposition.Kind == "follow-ups" {
			if err = verificationReady(ctx, tx, item, meta.Candidate); err != nil {
				return err
			}
		}
		if state.Disposition != nil && !ownerAccept {
			return reviewConflict("disposition already recorded")
		}
		if meta.Disposition == "accept" {
			if err = verificationReady(ctx, tx, item, meta.Candidate); err != nil {
				return err
			}
			if err = reviewReady(state, item.ScopeRevision, meta.Candidate); err != nil {
				return err
			}
			if receiptGated(state, meta.Candidate) {
				if _, err = exactReceipt(ctx, tx, item, meta.Candidate); err != nil {
					return err
				}
			}
		} else if !validGitCommit(meta.Candidate) {
			return reviewConflict("exact disposition candidate required")
		}
		state.Disposition = &api.ReviewDisposition{Kind: meta.Disposition, Candidate: meta.Candidate, MessageSeq: m.Seq, AgentID: req.AgentID, RunID: req.RunID}
		state.Dispositions = append(state.Dispositions, *state.Disposition)
		return saveReviewStage(ctx, tx, m.TaskID, earlier, state)
	}
	sc := scopeFor(&state, item.ScopeRevision)
	if meta.Mode == "focused" {
		if sc == nil {
			return reviewConflict("current scope is not assigned")
		}
		if len(state.Rounds) != 2 || state.Rounds[1].ResultSeq == 0 {
			return reviewConflict("focused path requires two completed general reviews")
		}
		if !validGitCommit(meta.Candidate) || strings.TrimSpace(meta.Fix) == "" {
			return reviewConflict("focused verification needs exact fix and candidate")
		}
		// No blocker IDs on a changed candidate: a rebase equivalence verification
		// of the round-two candidate, or with treeDiffers a lead-named fix whose
		// exact delta the verifier confirms. The hub checks identity and shape; the
		// verifier attests the git fact. criterionIds may ride on any focused
		// request, or stand alone on the reviewed candidate, to settle round-two
		// partials by the passing receipt for the exact candidate.
		rebase := len(meta.BlockerIDs) == 0
		reviewed := state.Rounds[1].Candidate
		route := "rebase verification"
		if meta.TreeDiffers {
			route = "treeDiffers verification"
		}
		if e.Kind == "request" {
			if err = reviewLead(ctx, tx, m.TaskID, item.ID, req.AgentID, req.RunID); err != nil {
				return err
			}
			if target.ID == "" || target.RunID == "" {
				return reviewConflict("exact verifier required")
			}
			eligible := false
			for _, r := range state.Rounds {
				if r.ReviewerID == target.ID && r.ReviewerRun == target.RunID {
					eligible = true
				}
			}
			if !eligible && meta.VerificationItemID != "" {
				var n int
				err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings WHERE agent_id=? AND run_id=? AND item_task_id=? AND item_id=?`, target.ID, target.RunID, m.TaskID, meta.VerificationItemID).Scan(&n)
				if err != nil {
					return err
				}
				eligible = n == 1
				linked := false
				for _, l := range req.WorkItems {
					if l.ItemTaskID == m.TaskID && l.ItemID == meta.VerificationItemID && l.Relationship == "related" {
						linked = true
					}
				}
				eligible = eligible && linked
			}
			if !eligible {
				return reviewConflict("verifier must be original exact reviewer or bound to linked verification item")
			}
			open := outstandingOnCandidate(state, meta.Candidate)
			settling := map[string]bool{}
			for _, id := range meta.CriterionIDs {
				if settling[id] || sc.Criteria[id] == "" || verificationOwned(sc.VerificationCriteria, id) || state.Rounds[1].Verdicts[id] != "partial" {
					return reviewConflict("criterionIds must name distinct round-two partial criteria that are not verification-owned")
				}
				for _, b := range open {
					if b.Criterion == id {
						return reviewConflict("criterion " + id + " has an unresolved blocker; name its blocker ID instead")
					}
				}
				for _, blocker := range meta.BlockerIDs {
					if blocker == id {
						return reviewConflict("criterion ID " + id + " is also a named blocker ID")
					}
				}
				settling[id] = true
			}
			if meta.TreeDiffers && !rebase {
				return reviewConflict("treeDiffers verification names no blocker IDs; verify unresolved blockers by ID")
			}
			if rebase && (meta.Candidate != reviewed || meta.TreeDiffers || len(meta.CriterionIDs) == 0) {
				if meta.Candidate == reviewed {
					return reviewConflict(route + " needs a candidate other than the reviewed one")
				}
				if !strings.Contains(meta.Fix, reviewed) {
					return reviewConflict(route + " fix must name the exact reviewed candidate")
				}
				if err = reviewReadySettling(state, item.ScopeRevision, reviewed, meta.CriterionIDs); err != nil {
					return reviewConflict(route + " needs a converged review; name unresolved blocker IDs instead, and round-two partials in criterionIds")
				}
				if len(open) != 0 {
					return reviewConflict("unresolved blockers on rebased candidate; name them for focused verification")
				}
			}
			seen := map[string]bool{}
			for _, id := range meta.BlockerIDs {
				if _, ok := open[id]; !ok || seen[id] {
					return reviewConflict("focused verification must name distinct unresolved blocker IDs")
				}
				seen[id] = true
			}
			if len(meta.CriterionIDs) > 0 {
				if _, err = exactReceipt(ctx, tx, item, meta.Candidate); err != nil {
					return err
				}
			}
			if len(state.Focused) > 0 && state.Focused[len(state.Focused)-1].ResultSeq == 0 {
				return reviewConflict("focused verification pending")
			}
			ids := meta.BlockerIDs
			if rebase {
				ids = []string{} // stored as [], never null
			}
			var criteria []string
			if len(meta.CriterionIDs) > 0 {
				criteria = meta.CriterionIDs
			}
			state.Focused = append(state.Focused, api.FocusedReview{RequestSeq: m.Seq, Candidate: meta.Candidate, Fix: meta.Fix, BlockerIDs: ids, CriterionIDs: criteria, TreeDiffers: meta.TreeDiffers, ReviewerID: target.ID, ReviewerRun: target.RunID, VerificationItemID: meta.VerificationItemID})
			state.Disposition = nil
		} else if e.Kind == "result" {
			found := false
			for i := range state.Focused {
				f := &state.Focused[i]
				if reviewRequestSeq(f.RequestSeq, f.ActiveRequestSeq) != req.ReplyTo {
					continue
				}
				found = true
				if f.ResultSeq != 0 || f.ReviewerID != req.AgentID || f.ReviewerRun != req.RunID || f.Candidate != meta.Candidate || f.Fix != meta.Fix || (len(f.BlockerIDs)+len(meta.BlockerIDs) != 0 && !reflect.DeepEqual(f.BlockerIDs, meta.BlockerIDs)) || f.VerificationItemID != meta.VerificationItemID {
					return reviewConflict("focused result identity, fix or candidate mismatch")
				}
				if f.TreeDiffers != meta.TreeDiffers || (len(f.CriterionIDs)+len(meta.CriterionIDs) != 0 && !reflect.DeepEqual(f.CriterionIDs, meta.CriterionIDs)) {
					return reviewConflict("focused result criterionIds or treeDiffers mismatch")
				}
				// Status holds exactly: each blocker ID, each criterion ID, and for a
				// changed candidate with no blocker either equivalence (identical
				// tree) or delta (treeDiffers). The two are never interchangeable.
				keys := append(append([]string{}, f.BlockerIDs...), f.CriterionIDs...)
				proof := ""
				if len(f.BlockerIDs) == 0 && f.Candidate != reviewed {
					proof = "equivalence"
					if f.TreeDiffers {
						proof = "delta"
					}
					keys = append(keys, proof)
				}
				shape := "focused result verifies exactly the named fixes with evidence; Status reports pass or fail for exactly " + strings.Join(keys, ", ")
				if proof == "equivalence" && len(f.CriterionIDs) == 0 {
					shape = "rebase result reports exactly equivalence pass or fail"
				}
				if len(meta.Blockers) > 0 || len(meta.Findings) > 0 || len(e.Evidence) == 0 || len(e.Body.Status) != len(keys) {
					return reviewConflict(shape)
				}
				passed := true
				for _, key := range keys {
					if e.Body.Status[key] != "pass" && e.Body.Status[key] != "fail" {
						return reviewConflict(shape)
					}
					passed = passed && e.Body.Status[key] == "pass"
				}
				if proof != "" {
					proven := false
					for _, ev := range e.Evidence {
						proven = proven || (ev.Type == "command" && strings.Contains(ev.Value, reviewed) && strings.Contains(ev.Value, f.Candidate))
					}
					if !proven {
						if proof == "delta" {
							return reviewConflict("delta result needs command evidence naming the reviewed and changed candidates")
						}
						return reviewConflict("rebase result needs command evidence naming the reviewed and rebased candidates")
					}
				}
				settles := len(f.CriterionIDs) > 0
				for _, id := range f.CriterionIDs {
					settles = settles && e.Body.Status[id] == "pass"
				}
				if settles {
					if f.ReceiptGeneration, err = exactReceipt(ctx, tx, item, f.Candidate); err != nil {
						return err
					}
				}
				f.Passed = passed
				f.ResultSeq = m.Seq
			}
			if !found {
				return reviewConflict("no matching focused request")
			}
		} else {
			return reviewConflict("focused path uses REQUEST or RESULT")
		}
		return saveReviewStage(ctx, tx, m.TaskID, earlier, state)
	}
	if meta.Mode != "general" || e.Kind != "result" {
		return reviewConflict("invalid review transition")
	}
	var round *api.ReviewRound
	for i := range state.Rounds {
		if reviewRequestSeq(state.Rounds[i].RequestSeq, state.Rounds[i].ActiveRequestSeq) == req.ReplyTo {
			round = &state.Rounds[i]
		}
	}
	if round == nil || round.ResultSeq != 0 || round.ReviewerID != req.AgentID || round.ReviewerRun != req.RunID || round.Candidate != meta.Candidate {
		return reviewConflict("review result identity, scope or candidate mismatch")
	}
	sc = &api.ReviewScope{ScopeRevision: round.ScopeRevision, Criteria: roundCriteria(&state, *round), VerificationCriteria: roundVerificationCriteria(&state, *round)}
	if len(sc.Criteria) == 0 {
		return reviewConflict("round frozen criteria are missing")
	}
	if round.Number == 2 && state.Rounds[0].ResultSeq == 0 {
		return reviewConflict("first general review must complete before second result")
	}
	if err = completeVerdicts(sc.Criteria, sc.VerificationCriteria, e.Body.Status); err != nil {
		return err
	}
	seen := map[string]bool{}
	prior := map[string]api.ReviewFinding{}
	if round.Number == 2 {
		for _, b := range state.Rounds[0].Blockers {
			prior[b.ID] = b
		}
	}
	blockers := []api.ReviewFinding{}
	findings := append([]api.ReviewFinding{}, meta.Findings...)
	for _, b := range meta.Blockers {
		allowedCriteria := sc.Criteria
		old, known := prior[b.ID]
		if known && sc.Criteria[b.Criterion] == "" {
			allowedCriteria = roundCriteria(&state, state.Rounds[0])
		}
		if err = evidenceFinding(b, meta.Candidate, allowedCriteria, true); err != nil {
			return err
		}
		if b.Criterion != "" && sc.Criteria[b.Criterion] != "" && e.Body.Status[b.Criterion] != "fail" {
			return reviewConflict("blocker criterion must have failed verdict")
		}
		if round.Number == 2 {
			if known && (old.Criterion != b.Criterion || old.Regression != b.Regression) {
				return reviewConflict("stable blocker ID changed meaning")
			}
			if !known && !b.Regression {
				findings = append(findings, b)
				continue
			}
		}
		if seen[b.ID] {
			return reviewConflict("duplicate finding ID")
		}
		seen[b.ID] = true
		blockers = append(blockers, b)
	}
	scopeResolvedIDs := []string{}
	// Round two must explicitly resolve or retain EVERY prior blocker, including regressions.
	if round.Number == 2 {
		resolved := map[string]bool{}
		for _, id := range meta.BlockerIDs {
			if _, ok := prior[id]; !ok || resolved[id] || seen[id] {
				return reviewConflict("resolved IDs must name distinct prior blockers")
			}
			resolved[id] = true
		}
		for id := range prior {
			if !seen[id] && !resolved[id] {
				return reviewConflict("round two must retain or explicitly resolve " + id)
			}
		}
		for id := range resolved {
			old := prior[id]
			if old.Criterion != "" && sc.Criteria[old.Criterion] != roundCriteria(&state, state.Rounds[0])[old.Criterion] {
				if strings.TrimSpace(meta.Fix) == "" || len(e.Evidence) == 0 {
					return reviewConflict("removed criterion resolution needs scope-change reason and evidence")
				}
				scopeResolvedIDs = append(scopeResolvedIDs, id)
			} else if old.Criterion != "" && e.Body.Status[old.Criterion] != "pass" {
				return reviewConflict("resolved blocker criterion must pass")
			}
		}
	}
	for _, f := range findings {
		if f.Criterion != "" && e.Body.Status[f.Criterion] != "fail" {
			return reviewConflict("criterion finding must retain failed verdict; use exact focused verification")
		}
		if err = evidenceFinding(f, meta.Candidate, sc.Criteria, false); err != nil {
			return err
		}
		if seen[f.ID] {
			return reviewConflict("duplicate finding ID")
		}
		seen[f.ID] = true
		existing := false
		for _, saved := range state.FollowUps {
			if saved.Finding.ID == f.ID {
				if !reflect.DeepEqual(saved.Finding, f) {
					return reviewConflict("follow-up ID changed meaning")
				}
				existing = true
			}
		}
		if !existing {
			follow, err := s.fileReviewFollowUp(ctx, tx, item, f, m, by)
			if err != nil {
				return err
			}
			state.FollowUps = append(state.FollowUps, follow)
		}
	}
	sort.Strings(scopeResolvedIDs)
	round.ResultSeq = m.Seq
	round.CompletedAt = ts(s.now())
	round.Verdicts = e.Body.Status
	round.Blockers = blockers
	round.Findings = findings
	// Explicit resolutions are durable even for regression blockers.
	if round.Number == 2 && len(meta.BlockerIDs) > 0 {
		state.Focused = append(state.Focused, api.FocusedReview{RequestSeq: round.RequestSeq, ResultSeq: m.Seq, Candidate: round.Candidate, Fix: "round two blocker verification: " + meta.Fix, BlockerIDs: meta.BlockerIDs, ScopeResolvedIDs: scopeResolvedIDs, ReviewerID: req.AgentID, ReviewerRun: req.RunID, Passed: true})
	}
	return saveReviewStage(ctx, tx, m.TaskID, earlier, state)
}

// reviewFindingTitleRefusal names the finding whose title cannot become a work
// item title, in the words tt send uses for the same title. The reason never
// repeats the title, and control characters in the finding ID become spaces,
// so the text is safe to return to the client.
func reviewFindingTitleRefusal(f api.ReviewFinding) error {
	reason := fmt.Sprintf("must be a work item title of 1 to %d characters", api.MaxReviewFindingTitleLen)
	if problems := api.ReviewFindingTitleProblems(&api.ReviewMetadata{Findings: []api.ReviewFinding{f}}); len(problems) > 0 {
		reason = problems[0].Reason
	}
	id := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(f.ID, " "))
	return fmt.Errorf("%w: review.findings[%s].title: %s", api.ErrInvalid, id, reason)
}

func (s *Store) fileReviewFollowUp(ctx context.Context, tx *sql.Tx, parent api.WorkItem, f api.ReviewFinding, m api.Message, by api.Caller) (api.ReviewFollowUp, error) {
	out := api.ReviewFollowUp{Finding: f, MessageSeq: m.Seq}
	kind := f.Kind
	if kind == "" {
		kind = "bug"
	}
	if !validWorkItemKind(kind) {
		return out, api.ErrInvalid
	}
	if !validWorkItemTitle(f.Title) {
		return out, reviewFindingTitleRefusal(f)
	}
	raw, _ := json.Marshal(f)
	now := s.now()
	sender := api.Sender{AgentID: m.From.AgentID, Node: by.Node, User: by.User}
	item := api.WorkItem{ID: api.NewID("wi"), TaskID: parent.TaskID, Kind: kind, Title: f.Title, Description: fmt.Sprintf("Review follow-up from %s, message #%d. Held for triage.\n%s", parent.ID, m.Seq, raw), Status: "open", Priority: "normal", Revision: 1, ScopeRevision: 1, SourceMessageSeq: m.Seq, CreatedBy: sender, UpdatedBy: sender, CreatedAt: now, UpdatedAt: now}
	result, err := tx.ExecContext(ctx, `INSERT INTO work_items(id,task_id,kind,title,description,status,priority,revision,source_message_seq,created_agent,created_node,created_user,updated_agent,updated_node,updated_user,created_at,updated_at,narrative_scope_revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.ID, item.TaskID, item.Kind, item.Title, item.Description, item.Status, item.Priority, 1, m.Seq, sender.AgentID, sender.Node, sender.User, sender.AgentID, sender.Node, sender.User, ts(now), ts(now), 1)
	if err != nil {
		return out, err
	}
	item.Seq, _ = result.LastInsertId()
	fields := []string{"description", "kind", "priority", "sourceMessageSeq", "status", "title"}
	sort.Strings(fields)
	creationFields, _ := json.Marshal(map[string]any{"kind": item.Kind, "title": item.Title, "description": item.Description, "status": item.Status, "priority": item.Priority, "sourceMessageSeq": item.SourceMessageSeq})
	change, err := tx.ExecContext(ctx, `INSERT INTO work_item_changes(item_id,revision,kind,fields,agent_id,by_node,by_user,created_at) VALUES(?,1,'created',?,?,?,?,?)`, item.ID, string(creationFields), sender.AgentID, sender.Node, sender.User, ts(now))
	if err != nil {
		return out, err
	}
	seq, _ := change.LastInsertId()
	if err = insertWorkItemRevision(ctx, tx, revisionFromItem(item, "", "created", "native", fields), seq); err != nil {
		return out, err
	}
	if err = refreshWorkItemHistoryState(ctx, tx, item, ts(now)); err != nil {
		return out, err
	}
	if _, err = s.insertEvent(ctx, tx, item.TaskID, "work_item_created", sender.AgentID, item.Title, map[string]any{"itemId": item.ID, "reviewParent": parent.ID, "messageSeq": m.Seq}, by); err != nil {
		return out, err
	}
	out.ItemID = item.ID
	return out, nil
}

func reviewRequestSeq(original, active int64) int64 {
	if active != 0 {
		return active
	}
	return original
}

// Reassignment changes the exact reviewer run without allocating another round.
func (s *Store) reassignReview(ctx context.Context, tx *sql.Tx, original api.Message, reissue api.Message, target api.Agent) error {
	if original.Envelope == nil {
		return nil
	}
	for _, link := range original.WorkItems {
		if link.Relationship != "primary" {
			continue
		}
		state, err := reviewState(ctx, tx, link.ItemTaskID, link.ItemID)
		if err != nil {
			return err
		}
		changed := false
		for i := range state.Rounds {
			r := &state.Rounds[i]
			if reviewRequestSeq(r.RequestSeq, r.ActiveRequestSeq) == original.Seq {
				if r.ResultSeq != 0 {
					return reviewConflict("completed review cannot be reassigned")
				}
				r.ActiveRequestSeq = reissue.Seq
				r.ReviewerID = target.ID
				r.ReviewerRun = target.RunID
				changed = true
			}
		}
		for i := range state.Focused {
			f := &state.Focused[i]
			if reviewRequestSeq(f.RequestSeq, f.ActiveRequestSeq) == original.Seq {
				return reviewConflict("focused verifier identity is frozen; resolve the request before changing it")
			}
		}
		if changed {
			return saveReviewState(ctx, tx, link.ItemTaskID, state)
		}
	}
	return nil
}

func outstandingWithoutFocused(state api.ReviewConvergence) map[string]api.ReviewFinding {
	state.Focused = nil
	return outstanding(state)
}

// roundCriteria preserves the exact requested snapshot, even when the item has
// been edited or historical scope revision numbers were never recorded.
func roundCriteria(state *api.ReviewConvergence, r api.ReviewRound) map[string]string {
	if len(r.Criteria) > 0 {
		return r.Criteria
	}
	if sc := scopeFor(state, r.ScopeRevision); sc != nil {
		return sc.Criteria
	}
	return nil
}
func roundVerificationCriteria(state *api.ReviewConvergence, r api.ReviewRound) []string {
	if len(r.Criteria) > 0 {
		return r.VerificationCriteria
	}
	if sc := scopeFor(state, r.ScopeRevision); sc != nil {
		return sc.VerificationCriteria
	}
	return nil
}

// Reconciliation reserves ALL immutable legacy typed requests, in source order.
// Legacy verdicts are not guessed: the explicitly bound exact reviewer reattests each
// original candidate against its original criteria using the ordinary RESULT.
func (s *Store) reconcileLegacyReviews(ctx context.Context, tx *sql.Tx, m api.Message, req api.PostMessageRequest, item api.WorkItem, state *api.ReviewConvergence) error {
	meta := req.Envelope.Review
	if err := reviewLead(ctx, tx, m.TaskID, item.ID, req.AgentID, req.RunID); err != nil {
		return err
	}
	if state.History != "unknown" || len(state.Rounds) != 0 || scopeFor(state, item.ScopeRevision) == nil {
		return reviewConflict("reconciliation requires assigned unknown legacy history")
	}
	if strings.TrimSpace(meta.Fix) == "" || len(req.Envelope.Evidence) == 0 {
		return reviewConflict("legacy reconciliation needs source evidence and reason")
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT m.seq FROM messages m JOIN message_work_item_links l ON l.message_seq=m.seq WHERE m.task_id=? AND l.item_task_id=? AND l.item_id=? AND m.seq<? AND json_valid(m.envelope) AND json_extract(m.envelope,'$.kind')='review' ORDER BY m.seq`, m.TaskID, item.TaskID, item.ID, m.Seq)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 || !reflect.DeepEqual(ids, meta.LegacyRequests) {
		return reviewConflict("reconciliation must enumerate every native legacy review request in source order")
	}
	if len(ids) > 2 {
		return reviewConflict("legacy lifetime count exceeds cap; no override or fresh rounds permitted")
	}
	bindings := map[int64]api.LegacyReviewerBinding{}
	for _, b := range meta.LegacyReviewers {
		known := false
		for _, id := range ids {
			if id == b.RequestSeq {
				known = true
			}
		}
		if !known || bindings[b.RequestSeq].RequestSeq != 0 || b.ReviewerID == "" || b.ReviewerRun == "" {
			return reviewConflict("replacement reviewer binding must name distinct legacy source and exact run")
		}
		bindings[b.RequestSeq] = b
	}
	actualBindings := []api.LegacyReviewerBinding{}
	for _, id := range ids {
		source, err := loadMessage(tx, ctx, m.TaskID, id)
		if err != nil {
			return err
		}
		env := source.Envelope
		if env == nil || !numberedCriteria(env.Body.Acceptance) || !validGitCommit(env.Body.Candidate) || source.To == "" {
			return reviewConflict("legacy source lacks exact candidate, frozen criteria or reviewer; retain unknown history")
		}
		reviewerID := source.To
		binding, explicit := bindings[id]
		if explicit {
			reviewerID = binding.ReviewerID
		}
		reviewer, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND task_id=?`, reviewerID, m.TaskID))
		if err != nil || reviewer.RunID == "" || reviewer.Status == api.AgentClosed || reviewer.Status == api.AgentExited || reviewer.Status == api.AgentRetired {
			return reviewConflict("legacy reattestation needs explicit available replacement or original reviewer")
		}
		if explicit && binding.ReviewerRun != reviewer.RunID {
			return reviewConflict("replacement reviewer run is stale")
		}
		actualBindings = append(actualBindings, api.LegacyReviewerBinding{RequestSeq: id, ReviewerID: reviewer.ID, ReviewerRun: reviewer.RunID})
		state.Rounds = append(state.Rounds, api.ReviewRound{Number: len(state.Rounds) + 1, RequestSeq: id, SourceReviewerID: source.To, Candidate: env.Body.Candidate, Criteria: env.Body.Acceptance, ReviewerID: reviewer.ID, ReviewerRun: reviewer.RunID, StartedAt: ts(source.CreatedAt), ReconciliationSeq: m.Seq})
	}
	state.History = "recorded"
	state.Reconciliations = append(state.Reconciliations, api.ReviewReconciliation{MessageSeq: m.Seq, Reason: meta.Fix, Requests: ids, Reviewers: actualBindings, AgentID: req.AgentID, RunID: req.RunID})
	state.Disposition = nil
	return saveReviewState(ctx, tx, m.TaskID, *state)
}

func scopeResolved(f api.FocusedReview, id string) bool {
	for _, resolved := range f.ScopeResolvedIDs {
		if resolved == id {
			return true
		}
	}
	return false
}

// Both focused request eligibility and acceptance use the same exact-candidate
// projection. Earlier passes are retained but may be reverified on a final
// candidate; they never silently clear a failure on an unrelated commit.
func outstandingOnCandidate(state api.ReviewConvergence, candidate string) map[string]api.ReviewFinding {
	filtered := state
	filtered.Focused = nil
	for _, f := range state.Focused {
		if f.Candidate == candidate || (len(state.Rounds) == 2 && f.RequestSeq == state.Rounds[1].RequestSeq && f.ResultSeq == state.Rounds[1].ResultSeq) {
			filtered.Focused = append(filtered.Focused, f)
		}
	}
	return outstanding(filtered)
}
