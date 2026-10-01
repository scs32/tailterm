package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"maps"
	"slices"
	"strings"
	"time"
)

const releasesSchema = `CREATE TABLE IF NOT EXISTS release_jobs (
 task_id TEXT NOT NULL REFERENCES tasks(id),id TEXT NOT NULL,entry_id TEXT NOT NULL,
 state TEXT NOT NULL,generation INTEGER NOT NULL,record_json TEXT NOT NULL,
 PRIMARY KEY(task_id,id),UNIQUE(task_id,entry_id));
 CREATE UNIQUE INDEX IF NOT EXISTS release_project_fence ON release_jobs(task_id) WHERE state IN ('claimed','merged','blocked');
 CREATE TABLE IF NOT EXISTS release_action_receipts (
 task_id TEXT NOT NULL,request_id TEXT NOT NULL,payload_hash TEXT NOT NULL,record_json TEXT NOT NULL,
 PRIMARY KEY(task_id,request_id));
 CREATE TRIGGER IF NOT EXISTS release_receipt_no_update BEFORE UPDATE ON release_action_receipts BEGIN SELECT RAISE(ABORT,'immutable release receipt'); END;
 CREATE TRIGGER IF NOT EXISTS release_receipt_no_delete BEFORE DELETE ON release_action_receipts BEGIN SELECT RAISE(ABORT,'immutable release receipt'); END;
 CREATE TABLE IF NOT EXISTS release_hand_releases (
 task_id TEXT NOT NULL REFERENCES tasks(id),id TEXT NOT NULL,intervention_seq INTEGER NOT NULL,
 released_commit TEXT NOT NULL,record_json TEXT NOT NULL,
 PRIMARY KEY(task_id,id),UNIQUE(task_id,intervention_seq));
 CREATE TRIGGER IF NOT EXISTS release_hand_release_no_update BEFORE UPDATE ON release_hand_releases BEGIN SELECT RAISE(ABORT,'immutable hand release'); END;
 CREATE TRIGGER IF NOT EXISTS release_hand_release_no_delete BEFORE DELETE ON release_hand_releases BEGIN SELECT RAISE(ABORT,'immutable hand release'); END;`

var releaseTargetNames = []string{"hub", "bridge", "mini", "tailos"}

func releaseConflict(reason string) error {
	return fmt.Errorf("%w: release: %s", api.ErrConflict, reason)
}
func releaseLoad(ctx context.Context, q queryRower, task, id string) (api.ReleaseJob, error) {
	var j api.ReleaseJob
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record_json FROM release_jobs WHERE task_id=? AND id=?`, task, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return j, api.ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &j)
	}
	return j, err
}
func (s *Store) Releases(ctx context.Context, task string) ([]api.ReleaseJob, error) {
	out := []api.ReleaseJob{}
	rows, err := s.db.QueryContext(ctx, `SELECT record_json FROM release_jobs WHERE task_id=? ORDER BY rowid`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var j api.ReleaseJob
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if slices.ContainsFunc(out, func(j api.ReleaseJob) bool { return j.State == "claimed" }) {
		approvals, err := releaseMatrixApprovals(ctx, s.db, task)
		if err != nil {
			return nil, err
		}
		for i := range out {
			if out[i].State == "claimed" {
				out[i].MatrixApprovals = approvals
			}
		}
	}
	return out, nil
}

const matrixApprovalPrefix = "verification-matrix-approval:"

// releaseMatrixApproval reports whether the message at seq is an owner
// approval of exactly this matrix digest: owner-authored, not a system
// message, and its whole trimmed text the approval token (the rule
// validateVerificationPlan applies to a candidate plan).
func releaseMatrixApproval(ctx context.Context, q queryRower, task string, seq int64, digest string) bool {
	var approver, node, text string
	if seq <= 0 || !validContextDigest(digest) {
		return false
	}
	err := q.QueryRowContext(ctx, `SELECT from_agent,from_node,text FROM messages WHERE task_id=? AND seq=?`, task, seq).Scan(&approver, &node, &text)
	return err == nil && approver == "" && node != "system" && strings.TrimSpace(text) == matrixApprovalPrefix+digest
}

// releaseMatrixApprovals lists the project's owner matrix approvals, the
// newest message per digest, in message order.
func releaseMatrixApprovals(ctx context.Context, db *sql.DB, task string) ([]api.ReleaseMatrixApproval, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq,text FROM messages WHERE task_id=? AND from_agent='' AND from_node<>'system' AND text LIKE ? ORDER BY seq`, task, "%"+matrixApprovalPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.ReleaseMatrixApproval
	for rows.Next() {
		var seq int64
		var text string
		if err = rows.Scan(&seq, &text); err != nil {
			return nil, err
		}
		digest, ok := strings.CutPrefix(strings.TrimSpace(text), matrixApprovalPrefix)
		if !ok || !validContextDigest(digest) {
			continue
		}
		out = slices.DeleteFunc(out, func(a api.ReleaseMatrixApproval) bool { return a.Digest == digest })
		out = append(out, api.ReleaseMatrixApproval{Digest: digest, MessageSeq: seq})
	}
	return out, rows.Err()
}

// handReleaseCommit is a full lowercase 40-hex commit, the form the CLI
// proves in git before it records or cites a hand release.
func handReleaseCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
func releaseText(s string) bool {
	return s != "" && len(s) <= 512 && !strings.ContainsAny(s, "\x00\n\r")
}

// validHandRelease checks the owner-supplied fields and returns the record
// with its targets in the fixed target order.
func validHandRelease(h *api.HandRelease) (api.HandRelease, error) {
	if h == nil || h.InterventionSeq <= 0 || !handReleaseCommit(h.ReleasedCommit) || !releaseText(h.Release) || len(h.Targets) == 0 || len(h.Commits) == 0 || len(h.Commits) > 64 {
		return api.HandRelease{}, api.ErrInvalid
	}
	seen := map[string]bool{}
	for _, t := range h.Targets {
		if !slices.Contains(releaseTargetNames, t) || seen[t] {
			return api.HandRelease{}, api.ErrInvalid
		}
		seen[t] = true
	}
	out := api.HandRelease{InterventionSeq: h.InterventionSeq, ReleasedCommit: h.ReleasedCommit, Release: h.Release}
	for _, name := range releaseTargetNames {
		if seen[name] {
			out.Targets = append(out.Targets, name)
		}
	}
	commits := map[string]bool{}
	for _, c := range h.Commits {
		if !handReleaseCommit(c) || commits[c] {
			return api.HandRelease{}, api.ErrInvalid
		}
		commits[c] = true
	}
	out.Commits = append([]string(nil), h.Commits...)
	return out, nil
}

// RecordHandRelease stores the owner's immutable record of a hand release.
// Owner-only means a request without an agent identity, the rule owner
// interventions use; the hub trusts the owner's commits and targets, which
// the CLI proves in a local checkout before sending them.
func (s *Store) RecordHandRelease(ctx context.Context, task string, req api.ReleaseRequest, by api.Caller) (api.HandRelease, error) {
	zero := api.HandRelease{}
	if req.AgentID != "" || req.RunID != "" {
		return zero, releaseConflict("only the owner records a hand release")
	}
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) {
		return zero, api.ErrInvalid
	}
	record, err := validHandRelease(req.HandRelease)
	if err != nil {
		return zero, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	hash := verificationDigest(req)
	var prior, raw string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,record_json FROM release_action_receipts WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&prior, &raw)
	if err == nil {
		if hash != prior {
			return zero, releaseConflict("retry changed")
		}
		err = json.Unmarshal([]byte(raw), &zero)
		return zero, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, task).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	} else if err != nil {
		return zero, err
	}
	if state != api.TaskOpen {
		return zero, api.ErrClosed
	}
	var kind string
	err = tx.QueryRowContext(ctx, `SELECT kind FROM owner_interventions WHERE task_id=? AND message_seq=?`, task, record.InterventionSeq).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && kind != "release") {
		return zero, releaseConflict("owner release intervention required")
	}
	if err != nil {
		return zero, err
	}
	record.ID = api.NewID("hrl")
	record.TaskID = task
	caller := by
	record.RecordedBy = &caller
	record.CreatedAt = ts(s.now())
	b, err := json.Marshal(record)
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_hand_releases VALUES(?,?,?,?,?)`, task, record.ID, record.InterventionSeq, record.ReleasedCommit, string(b)); err != nil {
		return zero, releaseConflict("intervention already has a hand release")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_action_receipts VALUES(?,?,?,?)`, task, req.RequestID, hash, string(b)); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return record, nil
}

// HandReleases lists the project's hand release records in record order.
func (s *Store) HandReleases(ctx context.Context, task string) ([]api.HandRelease, error) {
	out := []api.HandRelease{}
	rows, err := s.db.QueryContext(ctx, `SELECT record_json FROM release_hand_releases WHERE task_id=? ORDER BY rowid`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var h api.HandRelease
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func handReleaseLoad(ctx context.Context, q queryRower, task, id string) (api.HandRelease, error) {
	var h api.HandRelease
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record_json FROM release_hand_releases WHERE task_id=? AND id=?`, task, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &h)
	}
	return h, err
}
func releaseDeployer(ctx context.Context, q queryRower, task, agent, run string) error {
	var role, status, current, seen string
	if err := q.QueryRowContext(ctx, `SELECT role,status,run_id,last_seen_at FROM agents WHERE task_id=? AND id=?`, task, agent).Scan(&role, &status, &current, &seen); err != nil || role != api.AgentRoleDeployment || current != run || run == "" || seen == "" || (status != api.AgentRunning && status != api.AgentDone) {
		return releaseConflict("available exact deployment run required")
	}
	when, err := time.Parse(time.RFC3339Nano, seen)
	if err != nil || time.Since(when) > 90*time.Second {
		return releaseConflict("deployment heartbeat stale")
	}
	return nil
}
func releaseCandidate(ctx context.Context, tx *sql.Tx, task, entry string) (api.TeamQueueEntry, api.VerificationPlan, api.VerificationReceipt, error) {
	var p api.VerificationPlan
	var r api.VerificationReceipt
	e, err := scanTeamQueue(tx.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND id=?`, task, entry))
	if err != nil {
		return e, p, r, err
	}
	if e.Acceptance == nil {
		return e, p, r, releaseConflict("handler acceptance required")
	}
	item, err := getWorkItem(tx, ctx, task, e.ItemID)
	if err != nil {
		return e, p, r, err
	}
	records, err := verificationRecords(ctx, tx, task, e.ItemID)
	if err != nil {
		return e, p, r, err
	}
	plan, receipt := currentVerification(records)
	if plan == nil || receipt == nil || plan.Commit != e.Acceptance.Commit || !verifiedRepository(plan.Repository, *e.Acceptance) || plan.BaseCommit != e.Acceptance.BaseCommit || plan.ScopeRevision != item.ScopeRevision || e.Acceptance.ItemRevision != item.Revision || item.Status != "done" {
		return e, p, r, releaseConflict("exact accepted SHA and current verification required")
	}
	if err = verificationEligible(*plan, *receipt); err != nil {
		return e, p, r, err
	}
	if _, err = validateVerificationKnownFailures(ctx, tx, *plan, item, receipt); err != nil {
		return e, p, r, err
	}
	if err = reviewCompletion(ctx, tx, item, plan.Commit); err != nil {
		return e, p, r, err
	}
	return e, *plan, *receipt, nil
}

// Claims have no time based expiry: a disconnected runner may still execute.
// A blocked or ambiguous job retains the project fence until explicit recovery;
// a claimed job with no effects can instead be set aside for a later job.
func (s *Store) ReleaseAction(ctx context.Context, task string, req api.ReleaseRequest) (api.ReleaseJob, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	zero := api.ReleaseJob{}
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) {
		return zero, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	hash := verificationDigest(req)
	var prior, raw string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,record_json FROM release_action_receipts WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&prior, &raw)
	if err == nil {
		if hash != prior {
			return zero, releaseConflict("retry changed")
		}
		err = json.Unmarshal([]byte(raw), &zero)
		return zero, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	var state, pause string
	var generation int64
	if err = tx.QueryRowContext(ctx, `SELECT status,pause_state,pause_generation FROM tasks WHERE id=?`, task).Scan(&state, &pause, &generation); err != nil {
		return zero, err
	}
	if state != api.TaskOpen || pause != api.ProjectPauseActive {
		return zero, releaseConflict("active project required")
	}
	if err = ackGate(ctx, tx, req.AgentID, req.RunID, 0, s.now()); err != nil {
		return zero, err
	}
	var j api.ReleaseJob
	if req.Operation == "enqueue" {
		if err = requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
			return zero, err
		}
		e, p, r, eerr := releaseCandidate(ctx, tx, task, req.EntryID)
		if eerr != nil {
			return zero, eerr
		}
		var existing string
		if err = tx.QueryRowContext(ctx, `SELECT id FROM release_jobs WHERE task_id=? AND entry_id=?`, task, req.EntryID).Scan(&existing); err == nil {
			return zero, releaseConflict("entry already enqueued; recover original request receipt")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return zero, err
		}
		j = api.ReleaseJob{ID: api.NewID("rel"), TaskID: task, EntryID: e.ID, ItemID: e.ItemID, ItemRevision: e.Acceptance.ItemRevision, ScopeRevision: p.ScopeRevision, OrderMessageSeq: p.OrderMessageSeq, Repository: p.Repository, BaseCommit: p.BaseCommit, Commit: p.Commit, VerificationDigest: verificationDigest(r), Plan: p, State: "verified", Generation: 1, PauseGeneration: generation}
	} else {
		j, err = releaseLoad(ctx, tx, task, req.JobID)
		if err != nil {
			return zero, err
		}
		if j.Generation != req.ExpectedGeneration {
			return zero, releaseConflict("generation changed")
		}
		if req.Operation == "reconcile" {
			if err = requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
				return zero, err
			}
			if err = reconcileRelease(ctx, tx, task, &j, req.Reconciliation, generation); err != nil {
				return zero, err
			}
		} else if req.Operation == "set-aside" {
			if err = requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
				return zero, err
			}
			if err = setAsideRelease(ctx, tx, task, &j, req.Reconciliation, generation); err != nil {
				return zero, err
			}
		} else if req.Operation == "inputs" {
			if err = requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
				return zero, err
			}
			if j.State != "claimed" || j.InputsDigest != "" || !validContextDigest(req.InputsDigest) || !validGitCommit(req.IntegratedCommit) || (req.IntegratedCommit != j.Commit && (j.IntegratedVerification == nil || req.IntegratedCommit != j.IntegratedCommit)) {
				return zero, releaseConflict("exact claimed integrated input binding required")
			}
			if j.PauseGeneration != generation {
				return zero, releaseConflict("project generation changed")
			}
			j.InputsCommit = req.IntegratedCommit
			j.InputsDigest = req.InputsDigest
		} else if req.Operation == "supersede" {
			if err = requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
				return zero, err
			}
			// Only a job no deployer ever touched, or a terminal refused job
			// without a receipt: the deployer refuses before publication, but
			// a handler reconcile may refuse a published (merged or blocked)
			// job, which the covering hand release then settles. A claimed,
			// merged or blocked job still holds host history that needs
			// reconcile, not supersession; so does a job the handler set
			// aside, which is verified again but carries its reconciliation.
			untouched := j.State == "verified" && j.AgentID == "" && j.IntegratedCommit == "" && len(j.Reconciliations) == 0
			if (!untouched && j.State != "refused") || j.Receipt != nil {
				return zero, releaseConflict("only a verified never-claimed job or a refused unreceipted job can be superseded")
			}
			sup := req.Supersession
			if sup == nil || !validGitCommit(sup.ReleasedCommit) || (sup.Release != "" && !releaseText(sup.Release)) || sup.HandReleaseID == "" || len(sup.HandReleaseID) > 64 || len(sup.Targets) > 0 {
				return zero, api.ErrInvalid
			}
			hand, herr := handReleaseLoad(ctx, tx, task, sup.HandReleaseID)
			if errors.Is(herr, sql.ErrNoRows) {
				return zero, releaseConflict("recorded hand release required")
			}
			if herr != nil {
				return zero, herr
			}
			if hand.ReleasedCommit != sup.ReleasedCommit || (sup.Release != "" && sup.Release != hand.Release) {
				return zero, releaseConflict("hand release record names another release")
			}
			if !slices.Contains(hand.Commits, j.Commit) {
				return zero, releaseConflict("hand release does not cover the job's commit")
			}
			j.Supersession = &api.ReleaseSupersession{ReleasedCommit: hand.ReleasedCommit, Release: hand.Release, HandReleaseID: hand.ID, Targets: hand.Targets, AgentID: req.AgentID, RunID: req.RunID}
			j.SettledAt = hand.CreatedAt
			j.State = "superseded"
		} else if req.Operation == "verification" {
			if err = requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
				return zero, err
			}
			if j.State != "claimed" || !validGitCommit(req.IntegratedCommit) || req.Verification == nil {
				return zero, releaseConflict("claimed job and integrated receipt required")
			}
			if req.Plan == nil {
				return zero, releaseConflict("integrated matrix plan required")
			}
			p := *req.Plan
			if p.Commit != req.IntegratedCommit || p.BaseCommit != j.BaseCommit || p.Repository != j.Repository || p.ApprovedMatrixDigest != p.MatrixDigest || p.VerifierAgentID != j.AgentID || p.VerifierRunID != j.RunID || p.ChecksDigest != verificationDigest(p.Checks) || p.ScopeRevision != j.ScopeRevision {
				return zero, releaseConflict("integrated plan binding mismatch")
			}
			// The integrated commit may carry a matrix changed since the job
			// was approved. Its plan then binds that digest only under an
			// owner approval of exactly it; the job's approval never carries
			// over, and the job's own plan is left as approved.
			matrixChanged := p.MatrixDigest != j.Plan.MatrixDigest
			if !matrixChanged && p.MatrixApprovalMessageSeq != j.Plan.MatrixApprovalMessageSeq {
				return zero, releaseConflict("integrated plan binding mismatch")
			}
			if matrixChanged && !releaseMatrixApproval(ctx, tx, task, p.MatrixApprovalMessageSeq, p.MatrixDigest) {
				return zero, releaseConflict(fmt.Sprintf("matrix digest changed %.8s -> %.8s; no approval", j.Plan.MatrixDigest, p.MatrixDigest))
			}
			if err = releaseDeployer(ctx, tx, task, j.AgentID, j.RunID); err != nil {
				return zero, err
			}
			var coverage []api.ReleaseCheckCoverage
			for _, required := range j.Plan.Checks {
				want := verificationDigest(required)
				found := false
				var wider, rebuilt *api.VerificationCheck
				for i, check := range p.Checks {
					if verificationDigest(check) == want {
						found = true
					} else if wider == nil && goRaceCovers(required, check) {
						wider = &p.Checks[i]
					} else if matrixChanged && rebuilt == nil && matrixChangeCovers(required, check) {
						rebuilt = &p.Checks[i]
					}
				}
				if !found && wider != nil {
					found = true
					coverage = append(coverage, api.ReleaseCheckCoverage{CheckID: required.ID, ApprovedDigest: want, IntegratedDigest: verificationDigest(*wider), Relation: "superset"})
				}
				if !found && rebuilt != nil {
					found = true
					coverage = append(coverage, api.ReleaseCheckCoverage{CheckID: required.ID, ApprovedDigest: want, IntegratedDigest: verificationDigest(*rebuilt), Relation: "matrix_changed"})
				}
				if !found {
					return zero, releaseConflict("integrated matrix omitted approved check " + required.ID)
				}
			}
			if err = verificationEligible(p, *req.Verification); err != nil {
				return zero, err
			}
			// The plan's own item fields are not bound here, so the closing
			// item is the job's, not whatever the submitted plan names.
			var self api.WorkItem
			if self, err = getWorkItem(tx, ctx, j.TaskID, j.ItemID); err != nil {
				return zero, err
			}
			if _, err = validateVerificationKnownFailures(ctx, tx, p, self, req.Verification); err != nil {
				return zero, err
			}
			j.IntegratedCommit = req.IntegratedCommit
			j.IntegratedVerification = req.Verification
			j.IntegratedPlan = req.Plan
			j.IntegratedCoverage = coverage
			j.IntegratedMatrix = nil
			if matrixChanged {
				j.IntegratedMatrix = &api.ReleaseMatrixChange{ApprovedDigest: j.Plan.MatrixDigest, IntegratedDigest: p.MatrixDigest, ApprovalMessageSeq: p.MatrixApprovalMessageSeq}
			}
		} else {
			if err = releaseDeployer(ctx, tx, task, req.AgentID, req.RunID); err != nil {
				return zero, err
			}
			if j.PauseGeneration != generation {
				return zero, releaseConflict("project generation changed")
			}
			if req.Operation != "claim" && (j.AgentID != req.AgentID || j.RunID != req.RunID) {
				return zero, releaseConflict("fence belongs to another exact run")
			}
			switch req.Operation {
			case "claim":
				if j.State != "verified" {
					return zero, releaseConflict("job cannot be taken over")
				}
				e, p, r, eerr := releaseCandidate(ctx, tx, task, j.EntryID)
				if eerr != nil {
					return zero, eerr
				}
				if e.Acceptance.ItemRevision != j.ItemRevision || p.Commit != j.Commit || verificationDigest(r) != j.VerificationDigest {
					return zero, releaseConflict("candidate changed since enqueue")
				}
				j.State = "claimed"
				j.AgentID = req.AgentID
				j.RunID = req.RunID
			case "check":
				if j.State != "claimed" && j.State != "merged" {
					return zero, releaseConflict("execution fence unavailable")
				}
			case "merged":
				if j.State != "claimed" || !validGitCommit(req.IntegratedCommit) {
					return zero, releaseConflict("claimed integrated commit required")
				}
				if req.IntegratedCommit != j.Commit && (j.IntegratedVerification == nil || j.IntegratedCommit != req.IntegratedCommit) {
					return zero, releaseConflict("changed integrated SHA requires handler imported matrix receipt")
				}
				j.IntegratedCommit = req.IntegratedCommit
				j.Published = true
				j.State = "merged"
			case "finish":
				if j.State != "merged" || j.Receipt != nil || req.Receipt == nil {
					return zero, releaseConflict("merged unreceipted job required")
				}
				r := req.Receipt
				if r.Version != 1 || r.JobID != j.ID || r.Commit != j.IntegratedCommit || r.VerificationDigest != j.VerificationDigest || (r.Outcome != "released" && r.Outcome != "rolled_back" && r.Outcome != "blocked") {
					return zero, api.ErrInvalid
				}
				targets := map[string]bool{}
				for _, t := range r.Targets {
					if (t.Target != "hub" && t.Target != "bridge" && t.Target != "mini" && t.Target != "tailos") || targets[t.Target] || !validContextDigest(t.ArtifactSHA256) || strings.ContainsAny(t.Release+t.Backup+t.Version+t.Deployment, "\x00\n\r") || len(t.Release+t.Backup+t.Version+t.Deployment) > 2048 {
						return zero, api.ErrInvalid
					}
					targets[t.Target] = true
					if (r.Outcome == "released" && (t.Outcome != "released" || t.Rollback != "")) || (r.Outcome == "rolled_back" && (t.Outcome != "rolled_back" || t.Rollback != "restored")) {
						return zero, releaseConflict("receipt outcome contradicts target")
					}
					if t.Rollback != "" && t.Rollback != "restored" && t.Rollback != "blocked" {
						return zero, api.ErrInvalid
					}
					if (t.Target == "hub" || t.Target == "bridge") && (t.Backup == "" || !validContextDigest(t.PreflightReceiptSHA256)) {
						return zero, releaseConflict("target backup and preflight pin required")
					}
					if t.Outcome != "released" && t.Outcome != "failed" && t.Outcome != "rolled_back" {
						return zero, api.ErrInvalid
					}
					if t.Backup != "" && !validContextDigest(t.BackupSHA256) {
						return zero, api.ErrInvalid
					}
				}
				if err = validateReleaseRefEffects(r); err != nil {
					return zero, err
				}
				j.Receipt = r
				j.State = r.Outcome
				j.SettledAt = ts(s.now())
			case "refuse":
				if j.State != "claimed" || j.Receipt != nil {
					return zero, releaseConflict("only an unpublished claim may refuse")
				}
				j.State = "refused"
			case "block":
				if j.State != "claimed" && j.State != "merged" {
					return zero, releaseConflict("owned execution required")
				}
				j.State = "blocked"
			default:
				return zero, api.ErrInvalid
			}
		}
		j.Generation++
	}
	b, err := json.Marshal(j)
	if err != nil {
		return zero, err
	}
	if req.Operation == "enqueue" {
		_, err = tx.ExecContext(ctx, `INSERT INTO release_jobs VALUES(?,?,?,?,?,?)`, task, j.ID, j.EntryID, j.State, j.Generation, string(b))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE release_jobs SET state=?,generation=?,record_json=? WHERE task_id=? AND id=?`, j.State, j.Generation, string(b), task, j.ID)
	}
	if err != nil {
		return zero, releaseConflict("project already fenced or ledger conflict")
	}
	if req.Operation == "set-aside" {
		// Behind every queued job: the runner and Releases follow rowid order.
		if _, err = tx.ExecContext(ctx, `UPDATE release_jobs SET rowid=(SELECT MAX(rowid)+1 FROM release_jobs) WHERE task_id=? AND id=?`, task, j.ID); err != nil {
			return zero, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_action_receipts VALUES(?,?,?,?)`, task, req.RequestID, hash, string(b)); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return j, nil
}

// validateReleaseRefEffects checks the tasks-hub effects a receipt reports: a
// revert only after a rollback or block, and a push only for a live release
// of the receipt's own commit. Neither carries captured output.
func validateReleaseRefEffects(r *api.ReleaseReceipt) error {
	if v := r.Revert; v != nil {
		if r.Outcome == "released" || (v.Outcome != "committed" && v.Outcome != "failed") || (v.Outcome == "committed" && !validGitCommit(v.Commit)) || (v.Commit != "" && !validGitCommit(v.Commit)) || len(v.BugRequestID) > 128 || strings.ContainsAny(v.BugRequestID, "\x00\n\r ") {
			return api.ErrInvalid
		}
	}
	if p := r.Push; p != nil {
		if r.Outcome != "released" || p.Commit != r.Commit || (p.Outcome != "pushed" && p.Outcome != "failed") || p.Remote == "" || len(p.Remote) > 128 || strings.ContainsAny(p.Remote, "\x00\n\r /:@") {
			return api.ErrInvalid
		}
	}
	return nil
}

// ValidateReleaseDatabase checks a rehearsal copy without returning user data.
func (s *Store) ValidateReleaseDatabase(ctx context.Context) error {
	var integrity string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return releaseConflict("migration integrity failure")
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return releaseConflict("migration foreign key failure")
	}
	return rows.Err()
}

// An integrated go-race covers the approved one when it differs only by
// testing more packages: the runner derives go-race packages from the paths
// changed since the approved base, so a cherry-pick onto a moved tasks-hub
// adds the intervening commits' packages. Same id, cwd, environment and flags
// (every argv element before the first package); every approved package, or
// "./..." for any list. An approved "./..." is covered only by "./...".
func goRaceCovers(approved, integrated api.VerificationCheck) bool {
	if approved.ID != "go-race" || integrated.ID != approved.ID || integrated.Cwd != approved.Cwd || !maps.Equal(integrated.Environment, approved.Environment) {
		return false
	}
	flags, want, ok := goRaceArgv(approved.Argv)
	integratedFlags, have, integratedOK := goRaceArgv(integrated.Argv)
	if !ok || !integratedOK || !slices.Equal(flags, integratedFlags) {
		return false
	}
	if slices.Contains(have, "./...") {
		return true
	}
	for _, pkg := range want {
		if !slices.Contains(have, pkg) {
			return false
		}
	}
	return true
}

// Under an owner-approved matrix change the new matrix may rebuild any check
// (timeouts, flags, environment), so an approved check is covered by the
// integrated check with the same ID. A go-race must still test every approved
// package, or "./...".
func matrixChangeCovers(approved, integrated api.VerificationCheck) bool {
	if integrated.ID != approved.ID {
		return false
	}
	if approved.ID != "go-race" {
		return true
	}
	_, want, ok := goRaceArgv(approved.Argv)
	_, have, integratedOK := goRaceArgv(integrated.Argv)
	if !ok || !integratedOK {
		return false
	}
	return slices.Contains(have, "./...") || !slices.ContainsFunc(want, func(pkg string) bool { return !slices.Contains(have, pkg) })
}

// goRaceArgv splits argv at its first package; every later element must be a
// package too, so a flag after the packages never passes as one.
func goRaceArgv(argv []string) (flags, packages []string, ok bool) {
	i := slices.IndexFunc(argv, func(a string) bool { return strings.HasPrefix(a, "./") })
	if i < 1 {
		return nil, nil, false
	}
	for _, a := range argv[i:] {
		if !strings.HasPrefix(a, "./") {
			return nil, nil, false
		}
	}
	return argv[:i], argv[i:], true
}

// Reconciliation refuses unknown/active execution. Exited-run rotation and a
// handler's hashed host inspection are both required before releasing a fence.
func reconcileRelease(ctx context.Context, tx *sql.Tx, task string, j *api.ReleaseJob, r *api.ReleaseReconciliation, generation int64) error {
	if r == nil || r.JobID != j.ID || r.AgentID != j.AgentID || r.RunID != j.RunID || r.PauseGeneration != j.PauseGeneration || !validContextDigest(r.IncidentDigest) || !validContextDigest(r.JournalDigest) || !r.NoActiveExecution || !r.RefResolved || (r.JournalState != "no_effects" && r.JournalState != "restored") || (r.Disposition != "requeue" && r.Disposition != "refuse") {
		return releaseConflict("exact inspected recovery evidence required")
	}
	if j.State != "verified" && j.State != "claimed" && j.State != "merged" && j.State != "blocked" {
		return releaseConflict("job is not held")
	}
	err := releaseIncident(ctx, tx, task, r)
	if err != nil {
		return err
	}
	if j.AgentID != "" {
		var run, status string
		if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE task_id=? AND id=?`, task, j.AgentID).Scan(&run, &status); err != nil {
			return err
		}
		if run == j.RunID && status != api.AgentExited && status != api.AgentClosed {
			return releaseConflict("prior run has not exited; retirement is not termination")
		}
	}
	if r.Disposition == "requeue" {
		if !r.NoPublication || r.JournalState != "no_effects" || j.Receipt != nil {
			return releaseConflict("only inspected unpublished no-effect execution can requeue")
		}
		_, p, v, err := releaseCandidate(ctx, tx, task, j.EntryID)
		if err != nil {
			return err
		}
		if p.Commit != j.Commit || verificationDigest(v) != j.VerificationDigest {
			return releaseConflict("candidate changed")
		}
		j.State = "verified"
		j.AgentID = ""
		j.RunID = ""
		j.IntegratedCommit = ""
		j.IntegratedPlan = nil
		j.IntegratedCoverage = nil
		j.IntegratedMatrix = nil
		j.IntegratedVerification = nil
		j.InputsCommit = ""
		j.InputsDigest = ""
	} else {
		j.State = "refused"
	}
	j.PauseGeneration = generation
	j.Reconciliations = append(j.Reconciliations, *r)
	return nil
}

// releaseIncident checks the typed incident a reconcile or set-aside carries:
// its timestamps and text, a recorded incident bug, and a prevention item with
// a handler-confirmed order.
func releaseIncident(ctx context.Context, tx *sql.Tx, task string, r *api.ReleaseReconciliation) error {
	for _, stamp := range []string{r.ObservedAt, r.LastActionAt, r.StoppedAt} {
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
			return api.ErrInvalid
		}
	}
	if r.LockDigest != "" && !validContextDigest(r.LockDigest) {
		return api.ErrInvalid
	}
	for _, value := range []string{r.StopReason, r.LastAction, r.CausalEvidence, r.PreventionOwner, r.PreventionCriterion, r.ExpectedNextAction, r.ContributingConditions, r.UnresolvedQuestions} {
		if value == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\n\r") {
			return api.ErrInvalid
		}
	}
	incident, err := getWorkItem(tx, ctx, task, r.IncidentBugID)
	if err != nil || incident.Kind != "bug" {
		return releaseConflict("recorded incident bug required")
	}
	prevention, err := getWorkItem(tx, ctx, task, r.PreventionItemID)
	if err != nil {
		return err
	}
	return requireConfirmedTeamOrder(ctx, tx, task, prevention.ID, prevention.Revision, r.PreventionOrderMessage)
}

// setAsideRelease frees the fence held by a claimed job that has no effects,
// so a later verified job, often the fix for why this one cannot finish, can
// claim. A claimed job with no inputs binding cannot have published or
// deployed: the runner publishes only after the handler binds inputs. The
// claim's run need not have exited. The job stays verified and is claimed
// again, on the tasks-hub tip of that time, once the jobs ahead of it clear.
func setAsideRelease(ctx context.Context, tx *sql.Tx, task string, j *api.ReleaseJob, r *api.ReleaseReconciliation, generation int64) error {
	if r == nil || r.Disposition != "set_aside" || r.JobID != j.ID || r.AgentID != j.AgentID || r.RunID != j.RunID || r.PauseGeneration != j.PauseGeneration || !validContextDigest(r.IncidentDigest) || r.LockDigest != "" {
		return releaseConflict("exact set-aside record required")
	}
	if j.State != "claimed" || j.Published || j.Receipt != nil || j.InputsDigest != "" {
		return releaseConflict("only a claimed job with no effects can be set aside")
	}
	var waiting int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM release_jobs WHERE task_id=? AND state='verified' AND rowid>(SELECT rowid FROM release_jobs WHERE task_id=? AND id=?)`, task, task, j.ID).Scan(&waiting); err != nil {
		return err
	}
	if waiting == 0 {
		return releaseConflict("no later verified job is waiting on the fence")
	}
	if err := releaseIncident(ctx, tx, task, r); err != nil {
		return err
	}
	j.State = "verified"
	j.AgentID = ""
	j.RunID = ""
	j.IntegratedCommit = ""
	j.IntegratedPlan = nil
	j.IntegratedCoverage = nil
	j.IntegratedMatrix = nil
	j.IntegratedVerification = nil
	j.InputsCommit = ""
	j.InputsDigest = ""
	j.PauseGeneration = generation
	j.Reconciliations = append(j.Reconciliations, *r)
	return nil
}
