package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
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
 CREATE TRIGGER IF NOT EXISTS release_receipt_no_delete BEFORE DELETE ON release_action_receipts BEGIN SELECT RAISE(ABORT,'immutable release receipt'); END;`

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
	return out, rows.Err()
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
	if plan == nil || receipt == nil || plan.Commit != e.Acceptance.Commit || plan.Repository != e.Acceptance.Repository || plan.BaseCommit != e.Acceptance.BaseCommit || plan.ScopeRevision != item.ScopeRevision || e.Acceptance.ItemRevision != item.Revision || item.Status != "done" {
		return e, p, r, releaseConflict("exact accepted SHA and current verification required")
	}
	if err = verificationEligible(*plan, *receipt); err != nil {
		return e, p, r, err
	}
	if err = validateVerificationKnownFailures(ctx, tx, *plan); err != nil {
		return e, p, r, err
	}
	if err = reviewCompletion(ctx, tx, item, plan.Commit); err != nil {
		return e, p, r, err
	}
	return e, *plan, *receipt, nil
}

// Claims have no time based expiry: a disconnected runner may still execute.
// A blocked or ambiguous job retains the project fence until explicit recovery.
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
		if req.Operation == "verification" {
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
			if p.Commit != req.IntegratedCommit || p.BaseCommit != j.BaseCommit || p.Repository != j.Repository || p.MatrixDigest != j.Plan.MatrixDigest || p.ApprovedMatrixDigest != p.MatrixDigest || p.MatrixApprovalMessageSeq != j.Plan.MatrixApprovalMessageSeq || p.VerifierAgentID != j.AgentID || p.VerifierRunID != j.RunID || p.ChecksDigest != verificationDigest(p.Checks) || p.ScopeRevision != j.ScopeRevision {
				return zero, releaseConflict("integrated plan binding mismatch")
			}
			if err = releaseDeployer(ctx, tx, task, j.AgentID, j.RunID); err != nil {
				return zero, err
			}
			for _, required := range j.Plan.Checks {
				found := false
				for _, check := range p.Checks {
					if verificationDigest(check) == verificationDigest(required) {
						found = true
					}
				}
				if !found {
					return zero, releaseConflict("integrated matrix omitted approved check")
				}
			}
			if err = validateVerificationKnownFailures(ctx, tx, p); err != nil {
				return zero, err
			}
			if err = verificationEligible(p, *req.Verification); err != nil {
				return zero, err
			}
			j.IntegratedCommit = req.IntegratedCommit
			j.IntegratedVerification = req.Verification
			j.IntegratedPlan = req.Plan
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
				j.Receipt = r
				j.State = r.Outcome
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_action_receipts VALUES(?,?,?,?)`, task, req.RequestID, hash, string(b)); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return j, nil
}
