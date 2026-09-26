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
func completeVerdicts(criteria, verdicts map[string]string) error {
	if len(criteria) != len(verdicts) {
		return reviewConflict("verdicts must cover every frozen criterion")
	}
	for key := range criteria {
		v := verdicts[key]
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
	if last.ResultSeq == 0 || last.ScopeRevision != scope {
		return reviewConflict("latest general review is incomplete or stale")
	}
	verdicts := map[string]string{}
	for k, v := range last.Verdicts {
		verdicts[k] = v
	}
	for _, f := range state.Focused {
		if f.Passed && f.Candidate == candidate {
			for _, id := range f.BlockerIDs {
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
	for key := range sc.Criteria {
		if verdicts[key] != "pass" {
			return reviewConflict("criterion " + key + " has not passed")
		}
	}
	if len(outstanding(state)) != 0 {
		return reviewConflict("unresolved blockers")
	}
	if candidate != last.Candidate {
		if len(state.Focused) == 0 {
			return reviewConflict("changed candidate needs exact focused verification")
		}
		f := state.Focused[len(state.Focused)-1]
		if !f.Passed || f.Candidate != candidate {
			return reviewConflict("changed candidate needs exact focused verification")
		}
	}
	return nil
}
func reviewCompletion(ctx context.Context, tx *sql.Tx, item api.WorkItem, candidate string) error {
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	if state.History == "unknown" {
		if len(state.Scopes) > 0 {
			return reviewConflict("legacy review history is unknown")
		}
		return nil
	}
	if state.Disposition == nil || state.Disposition.Kind != "accept" {
		return reviewConflict("saved lead acceptance required")
	}
	if candidate != "" && candidate != state.Disposition.Candidate {
		return reviewConflict("acceptance candidate mismatch")
	}
	return reviewReady(state, item.ScopeRevision, state.Disposition.Candidate)
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
		if sc != nil && !reflect.DeepEqual(sc.Criteria, e.Body.Acceptance) {
			return reviewConflict("criteria frozen; revision-checked scope update required")
		}
		if sc == nil {
			state.Scopes = append(state.Scopes, api.ReviewScope{ScopeRevision: item.ScopeRevision, ItemRevision: item.Revision, AssignmentSeq: m.Seq, Criteria: e.Body.Acceptance})
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
		return saveReviewState(ctx, tx, m.TaskID, state)
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
		if sc == nil || !reflect.DeepEqual(sc.Criteria, e.Body.Acceptance) {
			return reviewConflict("review must use frozen assignment criteria")
		}
		if !validGitCommit(e.Body.Candidate) || target.ID == "" || target.RunID == "" {
			return reviewConflict("frozen candidate and exact reviewer run required")
		}
		if meta != nil && (meta.Mode != "general" || (meta.Candidate != "" && meta.Candidate != e.Body.Candidate)) {
			return reviewConflict("REVIEW is always a general review")
		}
		state.Rounds = append(state.Rounds, api.ReviewRound{Number: len(state.Rounds) + 1, ScopeRevision: item.ScopeRevision, RequestSeq: m.Seq, Candidate: e.Body.Candidate, ReviewerID: target.ID, ReviewerRun: target.RunID, StartedAt: ts(s.now())})
		state.Disposition = nil
		return saveReviewState(ctx, tx, m.TaskID, state)
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
	if meta.Mode == "disposition" {
		if e.Kind != "notice" {
			return reviewConflict("disposition requires NOTICE")
		}
		if err = reviewLead(ctx, tx, m.TaskID, item.ID, req.AgentID, req.RunID); err != nil {
			return err
		}
		if meta.Disposition != "accept" && meta.Disposition != "owner-decision" && meta.Disposition != "follow-ups" {
			return reviewConflict("unknown disposition")
		}
		if len(state.Rounds) == 0 || state.Rounds[len(state.Rounds)-1].ResultSeq == 0 {
			return reviewConflict("completed review required for disposition")
		}
		if state.Disposition != nil {
			return reviewConflict("disposition already recorded")
		}
		if meta.Disposition == "accept" {
			if err = reviewReady(state, item.ScopeRevision, meta.Candidate); err != nil {
				return err
			}
		} else if !validGitCommit(meta.Candidate) {
			return reviewConflict("exact disposition candidate required")
		}
		state.Disposition = &api.ReviewDisposition{Kind: meta.Disposition, Candidate: meta.Candidate, MessageSeq: m.Seq, AgentID: req.AgentID, RunID: req.RunID}
		state.Dispositions = append(state.Dispositions, *state.Disposition)
		return saveReviewState(ctx, tx, m.TaskID, state)
	}
	sc := scopeFor(&state, item.ScopeRevision)
	if sc == nil {
		return reviewConflict("current scope is not assigned")
	}
	if meta.Mode == "focused" {
		if len(state.Rounds) != 2 || state.Rounds[1].ResultSeq == 0 {
			return reviewConflict("focused path requires two completed general reviews")
		}
		if !validGitCommit(meta.Candidate) || strings.TrimSpace(meta.Fix) == "" || len(meta.BlockerIDs) == 0 {
			return reviewConflict("focused verification needs exact fix, candidate and blocker IDs")
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
			open := outstanding(state)
			seen := map[string]bool{}
			for _, id := range meta.BlockerIDs {
				if _, ok := open[id]; !ok || seen[id] {
					return reviewConflict("focused verification must name distinct unresolved blocker IDs")
				}
				seen[id] = true
			}
			if len(state.Focused) > 0 && state.Focused[len(state.Focused)-1].ResultSeq == 0 {
				return reviewConflict("focused verification pending")
			}
			state.Focused = append(state.Focused, api.FocusedReview{RequestSeq: m.Seq, Candidate: meta.Candidate, Fix: meta.Fix, BlockerIDs: meta.BlockerIDs, ReviewerID: target.ID, ReviewerRun: target.RunID, VerificationItemID: meta.VerificationItemID})
			state.Disposition = nil
		} else if e.Kind == "result" {
			found := false
			for i := range state.Focused {
				f := &state.Focused[i]
				if reviewRequestSeq(f.RequestSeq, f.ActiveRequestSeq) != req.ReplyTo {
					continue
				}
				found = true
				if f.ResultSeq != 0 || f.ReviewerID != req.AgentID || f.ReviewerRun != req.RunID || f.Candidate != meta.Candidate || f.Fix != meta.Fix || !reflect.DeepEqual(f.BlockerIDs, meta.BlockerIDs) || f.VerificationItemID != meta.VerificationItemID {
					return reviewConflict("focused result identity, fix or candidate mismatch")
				}
				if len(meta.Blockers) > 0 || len(meta.Findings) > 0 || len(e.Evidence) == 0 || len(e.Body.Status) != len(f.BlockerIDs) {
					return reviewConflict("focused result verifies exactly the named fixes with evidence")
				}
				f.Passed = true
				for _, id := range f.BlockerIDs {
					if e.Body.Status[id] != "pass" && e.Body.Status[id] != "fail" {
						return reviewConflict("focused verdict must name each blocker")
					}
					f.Passed = f.Passed && e.Body.Status[id] == "pass"
				}
				f.ResultSeq = m.Seq
			}
			if !found {
				return reviewConflict("no matching focused request")
			}
		} else {
			return reviewConflict("focused path uses REQUEST or RESULT")
		}
		return saveReviewState(ctx, tx, m.TaskID, state)
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
	if round == nil || round.ResultSeq != 0 || round.ReviewerID != req.AgentID || round.ReviewerRun != req.RunID || round.Candidate != meta.Candidate || round.ScopeRevision != item.ScopeRevision {
		return reviewConflict("review result identity, scope or candidate mismatch")
	}
	if err = completeVerdicts(sc.Criteria, e.Body.Status); err != nil {
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
		if err = evidenceFinding(b, meta.Candidate, sc.Criteria, true); err != nil {
			return err
		}
		if b.Criterion != "" && e.Body.Status[b.Criterion] != "fail" {
			return reviewConflict("blocker criterion must have failed verdict")
		}
		if round.Number == 2 {
			old, known := prior[b.ID]
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
			if old.Criterion != "" && e.Body.Status[old.Criterion] != "pass" {
				return reviewConflict("resolved blocker criterion must pass")
			}
		}
	}
	for _, f := range findings {
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
	round.ResultSeq = m.Seq
	round.CompletedAt = ts(s.now())
	round.Verdicts = e.Body.Status
	round.Blockers = blockers
	// Explicit resolutions are durable even for regression blockers.
	if round.Number == 2 && len(meta.BlockerIDs) > 0 {
		state.Focused = append(state.Focused, api.FocusedReview{RequestSeq: round.RequestSeq, ResultSeq: m.Seq, Candidate: round.Candidate, Fix: "round two blocker verification", BlockerIDs: meta.BlockerIDs, ReviewerID: req.AgentID, ReviewerRun: req.RunID, Passed: true})
	}
	return saveReviewState(ctx, tx, m.TaskID, state)
}

func (s *Store) fileReviewFollowUp(ctx context.Context, tx *sql.Tx, parent api.WorkItem, f api.ReviewFinding, m api.Message, by api.Caller) (api.ReviewFollowUp, error) {
	out := api.ReviewFollowUp{Finding: f, MessageSeq: m.Seq}
	kind := f.Kind
	if kind == "" {
		kind = "bug"
	}
	if !validWorkItemKind(kind) || !validWorkItemTitle(f.Title) {
		return out, api.ErrInvalid
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
