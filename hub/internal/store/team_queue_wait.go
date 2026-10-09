package store

// Team queue shared-path waits (docs/project-queue.md, "Team queue
// shared-path waits"): a handler records that one queue entry waits for
// paths another entry owns, until that entry is accepted, its item is saved
// done, or its release job is released. The broker tick (QueueWaitSweep)
// tells the waiting lead once when the condition is met, reports a team that
// stays idle afterwards once, and reports a predecessor that can no longer
// meet the condition to the primary handler. A wait is a record and a notice
// trigger only: it holds no entry out of admission and changes no ownership
// check. A listing shows waits and never evaluates them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/scs32/tailterm/hub/internal/api"
)

// teamQueueWaitsSQL creates the wait table. Open states stay in the partial
// index; final states leave it, so the tick's read stays small.
const teamQueueWaitsSQL = `CREATE TABLE IF NOT EXISTS team_queue_waits(
 task_id TEXT NOT NULL REFERENCES tasks(id),
 entry_id TEXT NOT NULL,
 item_id TEXT NOT NULL,
 on_entry_id TEXT NOT NULL,
 on_item_id TEXT NOT NULL,
 paths_json TEXT NOT NULL,
 until TEXT NOT NULL CHECK(until IN ('accepted','done','released')),
 state TEXT NOT NULL CHECK(state IN ('waiting','met','resumed','overdue','orphaned','cleared')),
 set_by TEXT NOT NULL DEFAULT '', set_at TEXT NOT NULL,
 met_at TEXT NOT NULL DEFAULT '', met_commit TEXT NOT NULL DEFAULT '', notice_seq INTEGER NOT NULL DEFAULT 0,
 helper_seq INTEGER NOT NULL DEFAULT 0, handler_seq INTEGER NOT NULL DEFAULT 0,
 reason TEXT NOT NULL DEFAULT '', closed_at TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(task_id,entry_id,on_entry_id));
 CREATE INDEX IF NOT EXISTS team_queue_waits_open ON team_queue_waits(task_id) WHERE state IN ('waiting','met','orphaned');`

const (
	queueWaitMetSubject      = "A shared path this team waited on is now free"
	queueWaitOrphanedSubject = "A shared path wait needs a handler decision"
	queueWaitOverdueSubject  = "A team is idle after its shared path wait was met"

	queueWaitNoCommit = "none recorded"
	// queueWaitMaxReason caps wait_clear's reason.
	queueWaitMaxReason = 500
	// queueWaitPathsBytes caps the path list in a notice body, which the
	// envelope limits to 4 KiB.
	queueWaitPathsBytes = 1500
)

// queueWaitIdleBound is how long a team may stay idle after its wait was met
// before the owner helper and the primary handler are told.
func queueWaitIdleBound() time.Duration {
	v, err := strconv.Atoi(os.Getenv("TAILTERM_QUEUE_WAIT_IDLE_MINUTES"))
	if err != nil || v < 1 || v > 1440 {
		v = 15
	}
	return time.Duration(v) * time.Minute
}

// queueWaitPathsText lists paths for a notice body, cut to fit it.
func queueWaitPathsText(paths []string) string {
	var b strings.Builder
	for i, p := range paths {
		if b.Len()+len(p) > queueWaitPathsBytes && i > 0 {
			fmt.Fprintf(&b, " and %d more", len(paths)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p)
	}
	return b.String()
}

func sameQueueWaitPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// queueWaitAuthority admits the owner (no agent fields) or a live database
// handler run of the project, and returns who set the wait. An item lead may
// neither set nor clear a wait.
func queueWaitAuthority(ctx context.Context, tx *sql.Tx, task string, req api.TeamQueueRequest) (string, error) {
	refused := fmt.Errorf("%w: only the owner or a database handler may set or clear a shared-path wait; ask the database handler", api.ErrConflict)
	if req.LeadAgentID != "" || req.LeadRunID != "" {
		return "", refused
	}
	if req.HandlerAgentID == "" && req.HandlerRunID == "" {
		return "owner", nil
	}
	var role, run, status string
	err := tx.QueryRowContext(ctx, `SELECT role,run_id,status FROM agents WHERE task_id=? AND id=?`, task, req.HandlerAgentID).Scan(&role, &run, &status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err != nil || role != api.AgentRoleDatabaseHandler || run != req.HandlerRunID || status == api.AgentClosed || status == api.AgentExited || status == api.AgentRetired {
		return "", refused
	}
	return req.HandlerAgentID, nil
}

// teamQueueWaitAction runs wait_set and wait_clear inside the queue action's
// transaction. It writes only team_queue_waits: the waiting entry's row and
// revision are untouched, and the returned entry carries its open waits.
func (s *Store) teamQueueWaitAction(ctx context.Context, tx *sql.Tx, t api.Task, req api.TeamQueueRequest, now string) (api.TeamQueueEntry, error) {
	var zero api.TeamQueueEntry
	if !validTeamQueueID(req.EntryID) || !validTeamQueueID(req.WaitOnEntryID) {
		return zero, fmt.Errorf("%w: a wait names the waiting entry and the predecessor entry, both tqe_ID", api.ErrInvalid)
	}
	if req.EntryID == req.WaitOnEntryID {
		return zero, fmt.Errorf("%w: an entry cannot wait on itself", api.ErrInvalid)
	}
	e, err := scanTeamQueue(tx.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND id=?`, t.ID, req.EntryID))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	by, err := queueWaitAuthority(ctx, tx, t.ID, req)
	if err != nil {
		return zero, err
	}
	var state, until, pathsJSON string
	err = tx.QueryRowContext(ctx, `SELECT state,until,paths_json FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, t.ID, req.EntryID, req.WaitOnEntryID).Scan(&state, &until, &pathsJSON)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	switch req.Operation {
	case "wait_clear":
		if len(req.WaitPaths) != 0 || req.WaitUntil != "" {
			return zero, fmt.Errorf("%w: wait clear takes no paths and no condition", api.ErrInvalid)
		}
		if len(req.WaitReason) > queueWaitMaxReason || strings.ContainsFunc(req.WaitReason, unicode.IsControl) {
			return zero, fmt.Errorf("%w: a wait clear reason is one line of at most %d bytes", api.ErrInvalid, queueWaitMaxReason)
		}
		if !exists || state == api.TeamQueueWaitCleared {
			return zero, api.ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,closed_at=?,reason=CASE WHEN ?<>'' THEN ? ELSE reason END WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
			api.TeamQueueWaitCleared, now, req.WaitReason, req.WaitReason, t.ID, req.EntryID, req.WaitOnEntryID); err != nil {
			return zero, err
		}
	case "wait_set":
		if req.WaitReason != "" {
			return zero, fmt.Errorf("%w: wait set takes no reason", api.ErrInvalid)
		}
		if !api.ValidTeamQueueWaitUntil(req.WaitUntil) {
			return zero, fmt.Errorf("%w: a wait's condition is accepted, done or released", api.ErrInvalid)
		}
		paths, err := canonicalQueueOwnership(req.WaitPaths)
		if err != nil {
			return zero, err
		}
		if len(paths) == 0 {
			return zero, fmt.Errorf("%w: a wait needs at least one path", api.ErrInvalid)
		}
		if (e.State != "queued" && e.State != "launching" && e.State != "running") || e.ReleasedAt != "" {
			shown := e.State
			if e.ReleasedAt != "" {
				shown += " and released"
			}
			return zero, fmt.Errorf("%w: entry %s is %s; only a queued, launching or running entry can wait", api.ErrConflict, e.ID, shown)
		}
		// The predecessor's item is read from its entry, never from the request.
		var onItem string
		err = tx.QueryRowContext(ctx, `SELECT item_id FROM team_queue_entries WHERE task_id=? AND id=?`, t.ID, req.WaitOnEntryID).Scan(&onItem)
		if errors.Is(err, sql.ErrNoRows) {
			return zero, fmt.Errorf("%w: predecessor entry %s is not in this project's queue", api.ErrConflict, req.WaitOnEntryID)
		}
		if err != nil {
			return zero, err
		}
		if exists && state != api.TeamQueueWaitCleared {
			var saved []string
			_ = json.Unmarshal([]byte(pathsJSON), &saved)
			if state != api.TeamQueueWaitWaiting || until != req.WaitUntil || !sameQueueWaitPaths(saved, paths) {
				return zero, fmt.Errorf("%w: entry %s already has a %s wait on %s; clear it first: tt team queue wait clear --task %s --entry %s --on %s", api.ErrConflict, e.ID, state, req.WaitOnEntryID, t.ID, e.ID, req.WaitOnEntryID)
			}
			break // the identical wait is already recorded
		}
		encoded, _ := json.Marshal(paths)
		if _, err := tx.ExecContext(ctx, `DELETE FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, t.ID, e.ID, req.WaitOnEntryID); err != nil {
			return zero, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_queue_waits(task_id,entry_id,item_id,on_entry_id,on_item_id,paths_json,until,state,set_by,set_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			t.ID, e.ID, e.ItemID, req.WaitOnEntryID, onItem, string(encoded), req.WaitUntil, api.TeamQueueWaitWaiting, by, now); err != nil {
			return zero, err
		}
	default:
		return zero, api.ErrInvalid
	}
	waits, err := openQueueWaits(ctx, tx, t.ID, e.ID)
	if err != nil {
		return zero, err
	}
	e.Waits = waits[e.ID]
	return e, nil
}

// openQueueWaits reads the open waits of a project, or of one entry when
// entry is not empty, by waiting entry, oldest first. It is one read of the
// partial index.
func openQueueWaits(ctx context.Context, q queryRower, task, entry string) (map[string][]api.TeamQueueWait, error) {
	rows, err := q.QueryContext(ctx, `SELECT entry_id,on_item_id,on_entry_id,paths_json,until,state,set_at,met_at,met_commit,notice_seq,reason FROM team_queue_waits
 WHERE task_id=? AND state IN ('waiting','met','orphaned') AND (?='' OR entry_id=?) ORDER BY set_at,on_entry_id`, task, entry, entry)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]api.TeamQueueWait{}
	for rows.Next() {
		var id, paths string
		var w api.TeamQueueWait
		if err := rows.Scan(&id, &w.OnItemID, &w.OnEntryID, &paths, &w.Until, &w.State, &w.SetAt, &w.MetAt, &w.MetCommit, &w.NoticeSeq, &w.Reason); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(paths), &w.Paths); err != nil {
			return nil, err
		}
		out[id] = append(out[id], w)
	}
	return out, rows.Err()
}

// attachQueueWaits shows each loaded active entry's open waits and appends
// their sentences to its BlockReason, so the queue UI shows them. It only
// appends: other code reads the start of a queued entry's reason. It reads
// the recorded state and evaluates no condition.
func attachQueueWaits(ctx context.Context, q queryRower, task string, out *api.TeamQueueList) error {
	waits, err := openQueueWaits(ctx, q, task, "")
	if err != nil || len(waits) == 0 {
		return err
	}
	for i := range out.Entries {
		e := &out.Entries[i]
		if queueEntryIsHistory(*e) {
			continue
		}
		e.Waits = waits[e.ID]
		for _, w := range e.Waits {
			if e.BlockReason != "" {
				e.BlockReason += ". "
			}
			e.BlockReason += w.Text()
		}
	}
	return nil
}

// QueueWaitStep is one notice the wait sweep posted.
type QueueWaitStep struct {
	TaskID     string
	EntryID    string
	OnEntryID  string
	Action     string // wait-met, wait-overdue, wait-orphaned
	MessageSeq int64
}

// queueWaitRow is one open wait with what the sweep needs to judge it.
type queueWaitRow struct {
	task, entry, item, onEntry, onItem string
	paths                              []string
	until, state                       string
	metAt, metCommit                   string
	noticeSeq                          int64
	// The waiting entry; waitingState is empty when its row is gone.
	waitingState, waitingReleased, waitingOwner string
	// The predecessor entry, its item and its latest release job.
	predExists                                         bool
	predState, predReleased, predAcceptance, predOwner string
	itemStatus                                         string
	jobState, jobRecord                                string
}

// waitingEnded reports that the waiting entry is finished, released as
// failed, or removed: its wait has nothing left to tell anyone.
func (w queueWaitRow) waitingEnded() bool {
	return w.waitingState == "" || w.waitingState == "finished" || (w.waitingState == "failed" && w.waitingReleased != "" && w.waitingOwner == "")
}

func jsonCommit(raw string, keys ...string) string {
	var fields map[string]json.RawMessage
	if raw == "" || json.Unmarshal([]byte(raw), &fields) != nil {
		return ""
	}
	for _, key := range keys {
		var value string
		if json.Unmarshal(fields[key], &value) == nil && value != "" {
			return value
		}
	}
	return ""
}

// met reports whether the wait's condition holds, with the commit to rebase
// onto and its kind.
func (w queueWaitRow) met() (ok bool, commit, kind string) {
	accepted := jsonCommit(w.predAcceptance, "commit")
	switch w.until {
	case api.TeamQueueWaitAccepted:
		if w.predExists && w.predAcceptance != "" {
			return true, accepted, "accepted"
		}
	case api.TeamQueueWaitDone:
		if w.itemStatus == "done" {
			if accepted != "" {
				return true, accepted, "accepted"
			}
			return true, "", ""
		}
	case api.TeamQueueWaitReleased:
		if w.jobState == "released" {
			return true, jsonCommit(w.jobRecord, "integratedCommit", "commit"), "released"
		}
	}
	return false, "", ""
}

// orphaned names why the predecessor can no longer meet the condition, or
// returns "" while it still can. It is asked only when the wait is not met.
// A running predecessor, or a failed one not yet released, is still pending.
func (w queueWaitRow) orphaned() string {
	switch {
	case !w.predExists:
		return "the predecessor entry was removed from the queue"
	case w.predOwner != "":
		reason := "the owner integrated the predecessor entry outside the acceptance path"
		if commit := jsonCommit(w.predOwner, "commit"); commit != "" {
			reason += " as commit " + commit
		}
		return reason
	case w.predState == "failed" && w.predReleased != "":
		return "the predecessor entry failed and was released"
	case w.itemStatus == "dismissed":
		return "the predecessor item was dismissed"
	case w.until == api.TeamQueueWaitReleased && (w.jobState == "superseded" || w.jobState == "refused" || w.jobState == "rolled_back"):
		return "the predecessor's release job is " + strings.ReplaceAll(w.jobState, "_", " ")
	case w.predState == "finished":
		switch w.until {
		case api.TeamQueueWaitAccepted:
			return "the predecessor entry finished without acceptance"
		case api.TeamQueueWaitDone:
			return "the predecessor entry finished and its item is not saved done"
		default:
			if w.jobState == "" {
				return "the predecessor entry finished without a release job"
			}
		}
	}
	return ""
}

// QueueWaitSweep runs the broker tick's pass over shared-path waits at now.
// It reads every open wait of the open, unpaused projects in one query. A
// tick with nothing to change writes nothing; each wait that changes state
// commits its notice and its new state together, so a retried tick and a
// restarted hub read the durable state and post nothing twice.
func (s *Store) QueueWaitSweep(ctx context.Context, now time.Time) ([]QueueWaitStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.task_id,w.entry_id,w.item_id,w.on_entry_id,w.on_item_id,w.paths_json,w.until,w.state,w.met_at,w.met_commit,w.notice_seq,
 COALESCE(we.state,''),COALESCE(we.released_at,''),COALESCE(we.owner_integration_json,''),
 pe.id IS NOT NULL,COALESCE(pe.state,''),COALESCE(pe.released_at,''),COALESCE(pe.acceptance_json,''),COALESCE(pe.owner_integration_json,''),
 COALESCE(pi.status,''),COALESCE(rj.state,''),COALESCE(rj.record_json,'')
 FROM team_queue_waits w JOIN tasks t ON t.id=w.task_id
 LEFT JOIN team_queue_entries we ON we.task_id=w.task_id AND we.id=w.entry_id
 LEFT JOIN team_queue_entries pe ON pe.task_id=w.task_id AND pe.id=w.on_entry_id
 LEFT JOIN work_items pi ON pi.task_id=w.task_id AND pi.id=w.on_item_id
 LEFT JOIN release_jobs rj ON rj.task_id=w.task_id AND rj.entry_id=w.on_entry_id
  AND rj.rowid=(SELECT max(rowid) FROM release_jobs WHERE task_id=w.task_id AND entry_id=w.on_entry_id)
 WHERE w.state IN ('waiting','met','orphaned') AND t.status=? AND t.pause_state=?
 ORDER BY w.task_id,w.set_at,w.entry_id,w.on_entry_id`, api.TaskOpen, api.ProjectPauseActive)
	if err != nil {
		return nil, err
	}
	var open []queueWaitRow
	for rows.Next() {
		var w queueWaitRow
		var paths string
		if err := rows.Scan(&w.task, &w.entry, &w.item, &w.onEntry, &w.onItem, &paths, &w.until, &w.state, &w.metAt, &w.metCommit, &w.noticeSeq,
			&w.waitingState, &w.waitingReleased, &w.waitingOwner,
			&w.predExists, &w.predState, &w.predReleased, &w.predAcceptance, &w.predOwner, &w.itemStatus, &w.jobState, &w.jobRecord); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(paths), &w.paths); err != nil {
			rows.Close()
			return nil, err
		}
		open = append(open, w)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var steps []QueueWaitStep
	var firstErr error
	for _, w := range open {
		step, err := s.sweepQueueWait(ctx, w, now)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("queue wait sweep %s on %s: %w", w.entry, w.onEntry, err)
		}
		if step != nil {
			steps = append(steps, *step)
		}
	}
	return steps, firstErr
}

// sweepQueueWait takes at most one step for one wait.
func (s *Store) sweepQueueWait(ctx context.Context, w queueWaitRow, now time.Time) (*QueueWaitStep, error) {
	if w.waitingEnded() {
		return nil, s.queueWaitTx(ctx, w, func(tx *sql.Tx, _ api.Task) (bool, error) {
			_, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,closed_at=?,reason=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
				api.TeamQueueWaitCleared, ts(now), "the waiting entry ended", w.task, w.entry, w.onEntry)
			return false, err
		})
	}
	switch w.state {
	case api.TeamQueueWaitWaiting:
		if ok, commit, kind := w.met(); ok {
			return s.queueWaitMet(ctx, w, commit, kind, now)
		}
		if reason := w.orphaned(); reason != "" {
			return s.queueWaitOrphaned(ctx, w, reason, now)
		}
	case api.TeamQueueWaitMet:
		return s.queueWaitIdle(ctx, w, now)
	}
	return nil, nil
}

// queueWaitTx runs fn in a write transaction when the project is still open
// and unpaused and the wait is still in the state the sweep read. fn reports
// whether it posted a message.
func (s *Store) queueWaitTx(ctx context.Context, w queueWaitRow, fn func(*sql.Tx, api.Task) (bool, error)) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, w.task))
	if err != nil {
		return err
	}
	if task.Status != api.TaskOpen || task.PauseState != api.ProjectPauseActive {
		return nil
	}
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, w.task, w.entry, w.onEntry).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && state != w.state) {
		return nil // changed since the read; decided afresh next tick
	}
	if err != nil {
		return err
	}
	posted, err := fn(tx, task)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if posted {
		s.notify(w.task)
	}
	return nil
}

// postQueueWaitNotice posts one hub notice to an agent and returns its number.
func (s *Store) postQueueWaitNotice(ctx context.Context, tx *sql.Tx, task api.Task, to api.Agent, subject, text string, refs map[string]string) (int64, error) {
	if err := s.postBrokerNotice(ctx, tx, task, to, "", subject, subject, text, refs); err != nil {
		return 0, err
	}
	var seq int64
	err := tx.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=? AND to_agent=?`, task.ID, to.ID).Scan(&seq)
	return seq, err
}

func (w queueWaitRow) refs(activity string) map[string]string {
	return map[string]string{"activity": activity, "entry": w.entry, "item": w.item, "predecessor": w.onItem, "predecessorEntry": w.onEntry, "condition": w.until}
}

// queueWaitMet tells the waiting entry's lead that the condition is met and
// marks the wait met in the same transaction. With no live lead yet, the
// wait stays waiting and the next tick tries again.
func (s *Store) queueWaitMet(ctx context.Context, w queueWaitRow, commit, kind string, now time.Time) (*QueueWaitStep, error) {
	var step *QueueWaitStep
	err := s.queueWaitTx(ctx, w, func(tx *sql.Tx, task api.Task) (bool, error) {
		lead, err := queueEntryLead(ctx, tx, task, w.item)
		if err != nil || lead.ID == "" {
			return false, err
		}
		shown, rebase := queueWaitNoCommit, "Rebase onto the integrated branch before editing these paths."
		if commit != "" {
			shown, rebase = kind+" commit "+commit, "Rebase onto that commit before editing these paths."
		}
		text := fmt.Sprintf("Queue entry %s (item %s) waited on item %s (entry %s) until it was %s. That condition is now met. Commit: %s. Paths now free: %s. %s",
			w.entry, w.item, w.onItem, w.onEntry, w.until, shown, queueWaitPathsText(w.paths), rebase)
		refs := w.refs("queue_wait")
		refs["commit"] = queueWaitNoCommit
		if commit != "" {
			refs["commit"] = commit
		}
		seq, err := s.postQueueWaitNotice(ctx, tx, task, lead, queueWaitMetSubject, text, refs)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,met_at=?,met_commit=?,notice_seq=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
			api.TeamQueueWaitMet, ts(now), commit, seq, w.task, w.entry, w.onEntry); err != nil {
			return false, err
		}
		step = &QueueWaitStep{TaskID: w.task, EntryID: w.entry, OnEntryID: w.onEntry, Action: "wait-met", MessageSeq: seq}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return step, nil
}

// queueWaitHandler is the project's primary handler when it can be told: a
// missing or retired one is no recipient.
func queueWaitHandler(ctx context.Context, tx *sql.Tx, task api.Task) (api.Agent, error) {
	handler, err := primaryHandler(ctx, tx, task)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && handler.Status == api.AgentRetired) {
		return api.Agent{}, nil
	}
	return handler, err
}

// queueWaitOrphaned reports a wait whose predecessor can no longer meet its
// condition to the primary handler for a decision. The waiting lead is told
// nothing and the wait is not released.
func (s *Store) queueWaitOrphaned(ctx context.Context, w queueWaitRow, reason string, now time.Time) (*QueueWaitStep, error) {
	var step *QueueWaitStep
	err := s.queueWaitTx(ctx, w, func(tx *sql.Tx, task api.Task) (bool, error) {
		handler, err := queueWaitHandler(ctx, tx, task)
		if err != nil || handler.ID == "" {
			return false, err
		}
		text := fmt.Sprintf("Queue entry %s (item %s) waits on item %s (entry %s) for %s until it is %s, but %s, so that condition cannot be met. "+
			"Decide: clear the wait with tt team queue wait clear --task %s --entry %s --on %s and tell the waiting lead, "+
			"or clear it and set a new one with tt team queue wait set --task %s --entry %s --on PREDECESSOR_ENTRY --owns PATH --until accepted|done|released. "+
			"The waiting lead was not told and nothing was released.",
			w.entry, w.item, w.onItem, w.onEntry, queueWaitPathsText(w.paths), w.until, reason, w.task, w.entry, w.onEntry, w.task, w.entry)
		seq, err := s.postQueueWaitNotice(ctx, tx, task, handler, queueWaitOrphanedSubject, text, w.refs("queue_wait_orphaned"))
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,reason=?,handler_seq=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
			api.TeamQueueWaitOrphaned, reason, seq, w.task, w.entry, w.onEntry); err != nil {
			return false, err
		}
		step = &QueueWaitStep{TaskID: w.task, EntryID: w.entry, OnEntryID: w.onEntry, Action: "wait-orphaned", MessageSeq: seq}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return step, nil
}

// queueWaitTeamIdle reports whether the waiting team has done nothing since
// its wait notice: no message from a live member after it, and every live
// member done, idle or finished silent (the stall check's member rule).
func (s *Store) queueWaitTeamIdle(ctx context.Context, w queueWaitRow) (bool, error) {
	const members = `SELECT b.agent_id FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id
 WHERE b.item_task_id=? AND b.item_id=? AND a.role<>? AND a.status NOT IN ('closed','exited')`
	var posted int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE task_id=? AND seq>? AND from_agent IN (`+members+`)`,
		w.task, w.noticeSeq, w.task, w.item, api.AgentRoleDatabaseHandler).Scan(&posted); err != nil || posted > 0 {
		return false, err
	}
	var busy int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id
 LEFT JOIN agent_activity x ON x.agent_id=b.agent_id AND x.run_id=b.run_id
 WHERE b.item_task_id=? AND b.item_id=? AND a.role<>? AND a.status NOT IN ('closed','exited',?) AND COALESCE(x.state,'') NOT IN ('idle','finished_silent')`,
		w.task, w.item, api.AgentRoleDatabaseHandler, api.AgentDone).Scan(&busy)
	return busy == 0, err
}

// queueWaitIdle follows a met wait: a team that shows activity ends it as
// resumed; a team idle for the bound is reported once to the owner helper
// and the primary handler. With neither to tell, the wait stays met.
func (s *Store) queueWaitIdle(ctx context.Context, w queueWaitRow, now time.Time) (*QueueWaitStep, error) {
	idle, err := s.queueWaitTeamIdle(ctx, w)
	if err != nil {
		return nil, err
	}
	if !idle {
		return nil, s.queueWaitTx(ctx, w, func(tx *sql.Tx, _ api.Task) (bool, error) {
			_, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,closed_at=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`, api.TeamQueueWaitResumed, ts(now), w.task, w.entry, w.onEntry)
			return false, err
		})
	}
	bound := queueWaitIdleBound()
	if now.Sub(parseTS(w.metAt)) < bound {
		return nil, nil
	}
	var step *QueueWaitStep
	err = s.queueWaitTx(ctx, w, func(tx *sql.Tx, task api.Task) (bool, error) {
		handler, err := queueWaitHandler(ctx, tx, task)
		if err != nil {
			return false, err
		}
		helper, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?,?) ORDER BY created_at DESC,id DESC LIMIT 1`, w.task, api.AgentRoleOwnerHelper, api.AgentClosed, api.AgentExited, api.AgentRetired))
		if errors.Is(err, sql.ErrNoRows) {
			helper, err = api.Agent{}, nil
		}
		if err != nil || (handler.ID == "" && helper.ID == "") {
			return false, err
		}
		commit := w.metCommit
		if commit == "" {
			commit = queueWaitNoCommit
		}
		text := fmt.Sprintf("Queue entry %s (item %s) was told in message %d at %s that its wait on item %s (entry %s) was met (%s, commit %s), and its team has been idle for %d minutes or more since. "+
			"Paths free: %s. Action: wake or message the waiting lead, or find out why the team has not resumed. Nothing was restarted or signalled.",
			w.entry, w.item, w.noticeSeq, w.metAt, w.onItem, w.onEntry, w.until, commit, int(bound/time.Minute), queueWaitPathsText(w.paths))
		refs := w.refs("queue_wait_overdue")
		refs["notice"] = strconv.FormatInt(w.noticeSeq, 10)
		var helperSeq, handlerSeq int64
		if helper.ID != "" {
			if helperSeq, err = s.postQueueWaitNotice(ctx, tx, task, helper, queueWaitOverdueSubject, text, refs); err != nil {
				return false, err
			}
		}
		if handler.ID != "" {
			if handlerSeq, err = s.postQueueWaitNotice(ctx, tx, task, handler, queueWaitOverdueSubject, text, refs); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,helper_seq=?,handler_seq=?,closed_at=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
			api.TeamQueueWaitOverdue, helperSeq, handlerSeq, ts(now), w.task, w.entry, w.onEntry); err != nil {
			return false, err
		}
		first := helperSeq
		if first == 0 {
			first = handlerSeq
		}
		step = &QueueWaitStep{TaskID: w.task, EntryID: w.entry, OnEntryID: w.onEntry, Action: "wait-overdue", MessageSeq: first}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return step, nil
}
