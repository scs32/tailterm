package store

// Team queue shared-path waits (docs/project-queue.md, "Team queue
// shared-path waits"): a handler records that one queue entry waits for
// paths another entry owns, until that entry is accepted, its item is saved
// done, or its release job is released. The broker tick (QueueWaitSweep)
// tells the waiting lead once when the condition is met, reports a team that
// stays idle afterwards once, and reports a predecessor that can no longer
// meet the condition to the primary handler. A wait is a record and a notice
// trigger only: it holds no entry out of admission and changes no ownership
// check. A listing shows waits and never evaluates them. A wait may name a
// reason instead of paths, for a requested pause that shares no path, such as
// a quiet host while the predecessor's release runs. A wait set with resume
// "handler" keeps its team paused after the met notice: the notice is
// evidence of the condition only, and the database handler's explicit resume
// is what restarts the team. Such a gated met wait, once past the idle bound,
// is reported once to the owner helper and the handler whatever its team
// does, and stays open until the handler clears it. A pathless wait may also
// wait on a release job (rel_ID) until its matrix departs or it settles, or
// on host admission (the key "host"), which only the handler's clear ends and
// which is reported the same way after queueWaitHostBound
// (docs/stall-watch.md, "Handler gate").

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
// index; final states leave it, so the tick's read stays small. on_entry_id
// is the predecessor key and on_kind its kind (entry, release or host); each
// kind has its own conditions. A host wait's handler resume is checked by
// wait_set, as the resume column predates on_kind.
const teamQueueWaitsSQL = `CREATE TABLE IF NOT EXISTS team_queue_waits(` + teamQueueWaitColumnsSQL + `);
 ` + teamQueueWaitsIndexSQL

const teamQueueWaitColumnsSQL = `
 task_id TEXT NOT NULL REFERENCES tasks(id),
 entry_id TEXT NOT NULL,
 item_id TEXT NOT NULL,
 on_entry_id TEXT NOT NULL,
 on_item_id TEXT NOT NULL,
 paths_json TEXT NOT NULL,
 on_kind TEXT NOT NULL DEFAULT 'entry' CHECK(on_kind IN ('entry','release','host')),
 until TEXT NOT NULL CHECK((on_kind='entry' AND until IN ('accepted','done','released')) OR (on_kind='release' AND until IN ('departed','settled')) OR (on_kind='host' AND until='handler')),
 state TEXT NOT NULL CHECK(state IN ('waiting','met','resumed','overdue','orphaned','cleared')),
 set_by TEXT NOT NULL DEFAULT '', set_at TEXT NOT NULL,
 met_at TEXT NOT NULL DEFAULT '', met_commit TEXT NOT NULL DEFAULT '', notice_seq INTEGER NOT NULL DEFAULT 0,
 helper_seq INTEGER NOT NULL DEFAULT 0, handler_seq INTEGER NOT NULL DEFAULT 0,
 reason TEXT NOT NULL DEFAULT '', closed_at TEXT NOT NULL DEFAULT '',
 resume TEXT NOT NULL DEFAULT '' CHECK(resume IN ('','handler')),
 PRIMARY KEY(task_id,entry_id,on_entry_id)`

const teamQueueWaitsIndexSQL = `CREATE INDEX IF NOT EXISTS team_queue_waits_open ON team_queue_waits(task_id) WHERE state IN ('waiting','met','orphaned');`

// teamQueueWaitCopyColumns are the columns every older wait table has, once
// the resume column is added: an on_kind rebuild copies them.
const teamQueueWaitCopyColumns = `task_id,entry_id,item_id,on_entry_id,on_item_id,paths_json,until,state,set_by,set_at,met_at,met_commit,notice_seq,helper_seq,handler_seq,reason,closed_at,resume`

// migrateTeamQueueWaits creates the wait table, adds the resume column to
// one created before it, and rebuilds one created before on_kind: SQLite
// cannot widen the old condition check in place. An older row reads as an
// automatic wait on an entry.
func migrateTeamQueueWaits(tx *sql.Tx) error {
	if _, err := tx.Exec(teamQueueWaitsSQL); err != nil {
		return err
	}
	column := func(name string) (bool, error) {
		var n int
		err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('team_queue_waits') WHERE name=?`, name).Scan(&n)
		return n > 0, err
	}
	hasResume, err := column("resume")
	if err != nil {
		return err
	}
	if !hasResume {
		if _, err := tx.Exec(`ALTER TABLE team_queue_waits ADD COLUMN resume TEXT NOT NULL DEFAULT '' CHECK(resume IN ('','handler'))`); err != nil {
			return err
		}
	}
	hasKind, err := column("on_kind")
	if err != nil || hasKind {
		return err
	}
	var before, after int
	if err := tx.QueryRow(`SELECT count(*) FROM team_queue_waits`).Scan(&before); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS team_queue_waits_v2;
		CREATE TABLE team_queue_waits_v2(` + teamQueueWaitColumnsSQL + `);
		INSERT INTO team_queue_waits_v2(` + teamQueueWaitCopyColumns + `,on_kind) SELECT ` + teamQueueWaitCopyColumns + `,'entry' FROM team_queue_waits;`); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM team_queue_waits_v2`).Scan(&after); err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("team_queue_waits rebuild copied %d of %d rows", after, before)
	}
	// The partial index goes with the old table and is made again.
	_, err = tx.Exec(`DROP TABLE team_queue_waits;
		ALTER TABLE team_queue_waits_v2 RENAME TO team_queue_waits;
		` + teamQueueWaitsIndexSQL)
	return err
}

const (
	queueWaitMetSubject      = "A shared path this team waited on is now free"
	queueWaitGatedSubject    = "A queue wait condition is met but the handler must still resume this team"
	queueWaitOrphanedSubject = "A shared path wait needs a handler decision"
	queueWaitOverdueSubject  = "A team is idle after its shared path wait was met"
	queueWaitGateSubject     = "A queue wait still needs the database handler's resume past its due time"

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

// queueWaitHostBound is how long a wait on host admission may stay open
// before the owner helper and the primary handler are told: the hub cannot
// see the host lock, so the handler's silence is bounded by time.
const queueWaitHostBound = 60 * time.Minute

// queueWaitJobSettled reports a release job state the job never leaves
// (api/releases.go, releaseSettled).
func queueWaitJobSettled(state string) bool {
	return state == "released" || state == "rolled_back" || state == "refused" || state == "superseded"
}

// queueWaitPathsText lists paths for a notice body, cut to fit it.
func queueWaitPathsText(paths []string) string {
	return api.TeamQueueWaitPaths(paths, queueWaitPathsBytes)
}

// pathless reports a wait that names a reason instead of paths: a requested
// pause that shares no path.
func (w queueWaitRow) pathless() bool { return len(w.paths) == 0 }

// gated reports a wait whose team stays paused after the met notice until
// the database handler resumes it.
func (w queueWaitRow) gated() bool { return w.resume == api.TeamQueueWaitResumeHandler }

// onText names the predecessor in a notice: an entry's item and entry, a
// release job and its item, or host admission.
func (w queueWaitRow) onText() string {
	switch w.onKind {
	case api.TeamQueueWaitOnRelease:
		if w.onItem == "" {
			return "release job " + w.onEntry
		}
		return fmt.Sprintf("release job %s (item %s)", w.onEntry, w.onItem)
	case api.TeamQueueWaitOnHost:
		return "host admission"
	}
	return fmt.Sprintf("item %s (entry %s)", w.onItem, w.onEntry)
}

// untilText is the condition in a notice, in the past tense.
func (w queueWaitRow) untilText() string {
	switch w.until {
	case api.TeamQueueWaitDeparted:
		return "its matrix departed"
	case api.TeamQueueWaitSettled:
		return "it settled"
	case api.TeamQueueWaitHandler:
		return "the database handler resumed it"
	}
	return "it was " + w.until
}

// waitedFor is what a notice says the wait holds: its paths, or a pathless
// wait's reason.
func (w queueWaitRow) waitedFor() string {
	if w.pathless() {
		return w.reason
	}
	return queueWaitPathsText(w.paths)
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
	kind := api.TeamQueueWaitOnKind(req.WaitOnEntryID)
	if !validTeamQueueID(req.EntryID) || kind == "" {
		return zero, fmt.Errorf("%w: a wait names the waiting entry and the predecessor entry, both tqe_ID, or a release job rel_ID or host as the predecessor", api.ErrInvalid)
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
	var state, savedUntil, pathsJSON, savedReason, savedResume string
	err = tx.QueryRowContext(ctx, `SELECT state,until,paths_json,reason,resume FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, t.ID, req.EntryID, req.WaitOnEntryID).Scan(&state, &savedUntil, &pathsJSON, &savedReason, &savedResume)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	switch req.Operation {
	case "wait_clear":
		if len(req.WaitPaths) != 0 || req.WaitUntil != "" || req.WaitResume != "" {
			return zero, fmt.Errorf("%w: wait clear takes no paths, no condition and no resume", api.ErrInvalid)
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
		if len(req.WaitReason) > queueWaitMaxReason || strings.ContainsFunc(req.WaitReason, unicode.IsControl) {
			return zero, fmt.Errorf("%w: a wait reason is one line of at most %d bytes", api.ErrInvalid, queueWaitMaxReason)
		}
		reason := strings.TrimSpace(req.WaitReason)
		until, resume := req.WaitUntil, req.WaitResume
		if kind == api.TeamQueueWaitOnHost {
			// Only the handler resumes a host wait: its condition and its
			// resume are both the handler's, stored as such.
			if until != "" && until != api.TeamQueueWaitHandler {
				return zero, fmt.Errorf("%w: a wait on host admission has one condition, the database handler's resume: omit --until or give handler", api.ErrInvalid)
			}
			if resume != "" && resume != api.TeamQueueWaitResumeHandler {
				return zero, fmt.Errorf("%w: a wait on host admission resumes only by the database handler: omit --resume or give handler", api.ErrInvalid)
			}
			until, resume = api.TeamQueueWaitHandler, api.TeamQueueWaitResumeHandler
		}
		if !api.ValidTeamQueueWaitCondition(kind, until) {
			if kind == api.TeamQueueWaitOnRelease {
				return zero, fmt.Errorf("%w: a wait on a release job's condition is departed or settled", api.ErrInvalid)
			}
			return zero, fmt.Errorf("%w: a wait's condition is accepted, done or released", api.ErrInvalid)
		}
		if !api.ValidTeamQueueWaitResume(resume) {
			return zero, fmt.Errorf("%w: a wait's resume is handler, or omitted for an automatic resume", api.ErrInvalid)
		}
		paths, err := canonicalQueueOwnership(req.WaitPaths)
		if err != nil {
			return zero, err
		}
		if kind != api.TeamQueueWaitOnEntry && (len(paths) > 0 || reason == "") {
			return zero, fmt.Errorf("%w: a wait on a release job or on host admission names a reason, not paths", api.ErrInvalid)
		}
		if len(paths) == 0 && reason == "" {
			return zero, fmt.Errorf("%w: a wait needs at least one path, or a reason for a pause that shares no path", api.ErrInvalid)
		}
		if len(paths) > 0 && reason != "" {
			return zero, fmt.Errorf("%w: a wait names shared paths or a reason, not both", api.ErrInvalid)
		}
		if (e.State != "queued" && e.State != "launching" && e.State != "running") || e.ReleasedAt != "" {
			shown := e.State
			if e.ReleasedAt != "" {
				shown += " and released"
			}
			return zero, fmt.Errorf("%w: entry %s is %s; only a queued, launching or running entry can wait", api.ErrConflict, e.ID, shown)
		}
		// The predecessor's item is read from its entry or its release job,
		// never from the request. Host admission has no item.
		onItem, err := queueWaitPredecessorItem(ctx, tx, t.ID, kind, req.WaitOnEntryID, until)
		if err != nil {
			return zero, err
		}
		if exists && state != api.TeamQueueWaitCleared {
			var saved []string
			_ = json.Unmarshal([]byte(pathsJSON), &saved)
			if state != api.TeamQueueWaitWaiting || savedUntil != until || !sameQueueWaitPaths(saved, paths) || savedReason != reason || savedResume != resume {
				return zero, fmt.Errorf("%w: entry %s already has a %s wait on %s; clear it first: tt team queue wait clear --task %s --entry %s --on %s", api.ErrConflict, e.ID, state, req.WaitOnEntryID, t.ID, e.ID, req.WaitOnEntryID)
			}
			break // the identical wait is already recorded
		}
		encoded, _ := json.Marshal(paths)
		if _, err := tx.ExecContext(ctx, `DELETE FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, t.ID, e.ID, req.WaitOnEntryID); err != nil {
			return zero, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_queue_waits(task_id,entry_id,item_id,on_entry_id,on_item_id,paths_json,on_kind,until,state,set_by,set_at,reason,resume) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, e.ID, e.ItemID, req.WaitOnEntryID, onItem, string(encoded), kind, until, api.TeamQueueWaitWaiting, by, now, reason, resume); err != nil {
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

// queueWaitPredecessorItem reads a new wait's predecessor item: the entry's
// item, or the release job's, refusing a job that has already met the
// condition. Host admission has no item.
func queueWaitPredecessorItem(ctx context.Context, tx *sql.Tx, task, kind, key, until string) (string, error) {
	var item string
	switch kind {
	case api.TeamQueueWaitOnEntry:
		err := tx.QueryRowContext(ctx, `SELECT item_id FROM team_queue_entries WHERE task_id=? AND id=?`, task, key).Scan(&item)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: predecessor entry %s is not in this project's queue", api.ErrConflict, key)
		}
		return item, err
	case api.TeamQueueWaitOnRelease:
		var job queueWaitRow
		err := tx.QueryRowContext(ctx, `SELECT state,record_json FROM release_jobs WHERE task_id=? AND id=?`, task, key).Scan(&job.jobState, &job.jobRecord)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: release job %s is not one of this project's release jobs", api.ErrConflict, key)
		}
		if err != nil {
			return "", err
		}
		job.onKind, job.until = kind, until
		if ok, _, _ := job.met(); ok {
			done := "has already settled"
			if !queueWaitJobSettled(job.jobState) {
				done = "has already departed the matrix"
			}
			return "", fmt.Errorf("%w: release job %s %s (state %s); there is nothing to wait for", api.ErrConflict, key, done, job.jobState)
		}
		return jsonCommit(job.jobRecord, "itemId"), nil
	}
	return "", nil
}

// openQueueWaits reads the open waits of a project, or of one entry when
// entry is not empty, by waiting entry, oldest first. It is one read of the
// partial index.
func openQueueWaits(ctx context.Context, q queryRower, task, entry string) (map[string][]api.TeamQueueWait, error) {
	rows, err := q.QueryContext(ctx, `SELECT entry_id,on_item_id,on_entry_id,on_kind,paths_json,until,state,set_at,met_at,met_commit,notice_seq,reason,resume FROM team_queue_waits
 WHERE task_id=? AND state IN ('waiting','met','orphaned') AND (?='' OR entry_id=?) ORDER BY set_at,on_entry_id`, task, entry, entry)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]api.TeamQueueWait{}
	for rows.Next() {
		var id, kind, paths string
		var w api.TeamQueueWait
		if err := rows.Scan(&id, &w.OnItemID, &w.OnEntryID, &kind, &paths, &w.Until, &w.State, &w.SetAt, &w.MetAt, &w.MetCommit, &w.NoticeSeq, &w.Reason, &w.Resume); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(paths), &w.Paths); err != nil {
			return nil, err
		}
		if kind != api.TeamQueueWaitOnEntry {
			w.OnKind = kind
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
	task, entry, item, onEntry, onItem, onKind string
	paths                                      []string
	until, state, reason, resume               string
	setAt, metAt, metCommit                    string
	noticeSeq, helperSeq, handlerSeq           int64
	// The waiting entry; waitingState is empty when its row is gone.
	waitingState, waitingReleased, waitingOwner string
	// hasLead: the waiting item has its own live team lead to tell.
	hasLead bool
	// The predecessor entry, its item and its latest release job; for a
	// release wait, the job itself.
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

// jsonHas reports whether a JSON object carries key with a value that is
// not null.
func jsonHas(raw, key string) bool {
	var fields map[string]json.RawMessage
	if raw == "" || json.Unmarshal([]byte(raw), &fields) != nil {
		return false
	}
	v, ok := fields[key]
	return ok && string(v) != "null"
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
	if w.onKind == api.TeamQueueWaitOnRelease {
		// Settled is final; departed is the integrated matrix result
		// imported, or a job that merged, blocked or settled. A job reset to
		// verified has not departed: its matrix runs again.
		settled := queueWaitJobSettled(w.jobState)
		departed := settled || w.jobState == "merged" || w.jobState == "blocked" || jsonHas(w.jobRecord, "integratedVerification")
		if (w.until == api.TeamQueueWaitSettled && settled) || (w.until == api.TeamQueueWaitDeparted && departed) {
			if c := jsonCommit(w.jobRecord, "integratedCommit"); c != "" {
				return true, c, "integrated"
			}
			if w.jobState == "released" {
				return true, jsonCommit(w.jobRecord, "commit"), "released"
			}
			return true, "", ""
		}
		return false, "", ""
	}
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
// A release job is pending while its row exists; host admission is never
// orphaned.
func (w queueWaitRow) orphaned() string {
	switch w.onKind {
	case api.TeamQueueWaitOnRelease:
		if w.jobState == "" {
			return "the release job was removed"
		}
		return ""
	case api.TeamQueueWaitOnHost:
		return ""
	}
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
// tick with nothing to change writes nothing and takes no write lock: a met
// wait whose entry has no lead yet is seen in that read, and a wait to
// report with no one to tell costs three more reads per project. Each wait
// that changes state commits its notice and its new state together, so a
// retried tick and a restarted hub read the durable state and post nothing
// twice.
func (s *Store) QueueWaitSweep(ctx context.Context, now time.Time) ([]QueueWaitStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.task_id,w.entry_id,w.item_id,w.on_entry_id,w.on_item_id,w.on_kind,w.paths_json,w.until,w.state,w.reason,w.resume,w.set_at,w.met_at,w.met_commit,w.notice_seq,w.helper_seq,w.handler_seq,
 COALESCE(we.state,''),COALESCE(we.released_at,''),COALESCE(we.owner_integration_json,''),la.id IS NOT NULL,
 pe.id IS NOT NULL,COALESCE(pe.state,''),COALESCE(pe.released_at,''),COALESCE(pe.acceptance_json,''),COALESCE(pe.owner_integration_json,''),
 COALESCE(pi.status,''),COALESCE(rj.state,''),COALESCE(rj.record_json,'')
 FROM team_queue_waits w JOIN tasks t ON t.id=w.task_id
 LEFT JOIN team_queue_entries we ON we.task_id=w.task_id AND we.id=w.entry_id
 LEFT JOIN item_team_leads l ON l.task_id=w.task_id AND l.item_id=w.item_id AND l.state<>'closed'
 LEFT JOIN agents la ON la.id=l.agent_id AND la.run_id=l.run_id AND la.task_id=w.task_id AND la.status NOT IN ('closed','exited')
 LEFT JOIN team_queue_entries pe ON pe.task_id=w.task_id AND pe.id=w.on_entry_id
 LEFT JOIN work_items pi ON pi.task_id=w.task_id AND pi.id=w.on_item_id
 LEFT JOIN release_jobs rj ON rj.task_id=w.task_id AND ((w.on_kind='release' AND rj.id=w.on_entry_id) OR (w.on_kind='entry' AND rj.entry_id=w.on_entry_id
  AND rj.rowid=(SELECT max(rowid) FROM release_jobs WHERE task_id=w.task_id AND entry_id=w.on_entry_id)))
 WHERE w.state IN ('waiting','met','orphaned') AND t.status=? AND t.pause_state=?
 ORDER BY w.task_id,w.set_at,w.entry_id,w.on_entry_id`, api.TaskOpen, api.ProjectPauseActive)
	if err != nil {
		return nil, err
	}
	var open []queueWaitRow
	for rows.Next() {
		var w queueWaitRow
		var paths string
		if err := rows.Scan(&w.task, &w.entry, &w.item, &w.onEntry, &w.onItem, &w.onKind, &paths, &w.until, &w.state, &w.reason, &w.resume, &w.setAt, &w.metAt, &w.metCommit, &w.noticeSeq, &w.helperSeq, &w.handlerSeq,
			&w.waitingState, &w.waitingReleased, &w.waitingOwner, &w.hasLead,
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
	// Whether a project has a primary handler, and an owner helper, to tell,
	// read at most once per sweep and only for a project with a wait to
	// report.
	type reach struct{ handler, helper bool }
	reached := map[string]reach{}
	tellers := func(task string) (bool, bool, error) {
		if known, ok := reached[task]; ok {
			return known.handler, known.helper, nil
		}
		t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
		if err != nil {
			return false, false, err
		}
		handler, err := queueWaitHandler(ctx, s.db, t)
		if err != nil {
			return false, false, err
		}
		var helpers int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?,?)`, task, api.AgentRoleOwnerHelper, api.AgentClosed, api.AgentExited, api.AgentRetired).Scan(&helpers); err != nil {
			return false, false, err
		}
		reached[task] = reach{handler.ID != "", helpers > 0}
		return handler.ID != "", helpers > 0, nil
	}
	for _, w := range open {
		step, err := s.sweepQueueWait(ctx, w, now, tellers)
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
func (s *Store) sweepQueueWait(ctx context.Context, w queueWaitRow, now time.Time, tellers func(string) (handler, helper bool, err error)) (*QueueWaitStep, error) {
	if w.waitingEnded() {
		return nil, s.queueWaitTx(ctx, w, func(tx *sql.Tx, _ api.Task) (bool, error) {
			_, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,closed_at=?,reason=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
				api.TeamQueueWaitCleared, ts(now), "the waiting entry ended", w.task, w.entry, w.onEntry)
			return false, err
		})
	}
	switch w.state {
	case api.TeamQueueWaitWaiting:
		if w.onKind == api.TeamQueueWaitOnHost {
			// Never met: only the handler's clear ends it.
			return s.queueWaitGateDue(ctx, w, now, tellers)
		}
		if ok, commit, kind := w.met(); ok {
			if !w.hasLead {
				// A queued entry has no team yet; its own lead is told once
				// it exists. Nothing is locked or written until then.
				return nil, nil
			}
			return s.queueWaitMet(ctx, w, commit, kind, now)
		}
		if reason := w.orphaned(); reason != "" {
			if ok, _, err := tellers(w.task); err != nil || !ok {
				return nil, err
			}
			return s.queueWaitOrphaned(ctx, w, reason, now)
		}
	case api.TeamQueueWaitMet:
		if w.gated() {
			return s.queueWaitGateDue(ctx, w, now, tellers)
		}
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
	refs := map[string]string{"activity": activity, "entry": w.entry, "item": w.item, "predecessor": w.onItem, "predecessorEntry": w.onEntry, "condition": w.until}
	if w.onKind != api.TeamQueueWaitOnEntry {
		refs["predecessorKind"] = w.onKind
	}
	if w.onItem == "" {
		delete(refs, "predecessor")
	}
	return refs
}

// queueWaitLead is the waiting item's own live team lead. It never falls
// back to the project orchestrator, which may lead another item: a queued
// entry's lead does not exist yet and must be the one told.
func queueWaitLead(ctx context.Context, tx *sql.Tx, task, item string) (api.Agent, error) {
	var agentID, runID string
	err := tx.QueryRowContext(ctx, `SELECT agent_id,run_id FROM item_team_leads WHERE task_id=? AND item_id=? AND state<>'closed'`, task, item).Scan(&agentID, &runID)
	if err == nil {
		var lead api.Agent
		lead, err = scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND run_id=? AND status NOT IN (?,?)`, task, agentID, runID, api.AgentClosed, api.AgentExited))
		if err == nil {
			return lead, nil
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return api.Agent{}, nil
	}
	return api.Agent{}, err
}

// queueWaitMet tells the waiting entry's lead that the condition is met and
// marks the wait met in the same transaction. With no live lead yet, the
// wait stays waiting and the next tick tries again.
func (s *Store) queueWaitMet(ctx context.Context, w queueWaitRow, commit, kind string, now time.Time) (*QueueWaitStep, error) {
	var step *QueueWaitStep
	err := s.queueWaitTx(ctx, w, func(tx *sql.Tx, task api.Task) (bool, error) {
		lead, err := queueWaitLead(ctx, tx, w.task, w.item)
		if err != nil || lead.ID == "" {
			return false, err
		}
		shown, rebase := queueWaitNoCommit, "Rebase onto the integrated branch before editing these paths."
		if commit != "" {
			shown, rebase = kind+" commit "+commit, "Rebase onto that commit before editing these paths."
		}
		text := fmt.Sprintf("Queue entry %s (item %s) waited on %s until %s. That condition is now met. Commit: %s. Paths now free: %s. %s",
			w.entry, w.item, w.onText(), w.untilText(), shown, queueWaitPathsText(w.paths), rebase)
		if w.pathless() {
			// No path to edit, so no rebase sentence: the paused work resumes.
			text = fmt.Sprintf("Queue entry %s (item %s) waited on %s until %s. That condition is now met. Commit: %s. Paused for: %s. Resume the paused work.",
				w.entry, w.item, w.onText(), w.untilText(), shown, w.reason)
		}
		subject := queueWaitMetSubject
		if w.gated() {
			// The condition is evidence only: the team stays paused until
			// the handler's explicit resume, so nothing here says to start.
			subject = queueWaitGatedSubject
			then := "Once the handler resumes this team, rebase onto the integrated branch before editing these paths."
			if commit != "" {
				then = "Once the handler resumes this team, rebase onto that commit before editing these paths."
			}
			text = fmt.Sprintf("Queue entry %s (item %s) waited on %s until %s. That condition is now met. Commit: %s. Paths now free: %s. "+
				"This wait also needs an explicit resume from the database handler: do not start on this notice. %s",
				w.entry, w.item, w.onText(), w.untilText(), shown, queueWaitPathsText(w.paths), then)
			if w.pathless() {
				text = fmt.Sprintf("Queue entry %s (item %s) waited on %s until %s. That condition is now met. Commit: %s. Paused for: %s. "+
					"This wait also needs an explicit resume from the database handler: stay paused and do not start on this notice; the handler sends the resume after its own checks.",
					w.entry, w.item, w.onText(), w.untilText(), shown, w.reason)
			}
		}
		refs := w.refs("queue_wait")
		refs["commit"] = queueWaitNoCommit
		if commit != "" {
			refs["commit"] = commit
		}
		if w.gated() {
			refs["resume"] = api.TeamQueueWaitResumeHandler
		}
		seq, err := s.postQueueWaitNotice(ctx, tx, task, lead, subject, text, refs)
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
func queueWaitHandler(ctx context.Context, q queryRower, task api.Task) (api.Agent, error) {
	handler, err := primaryHandler(ctx, q, task)
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
		// The re-set form matches the wait's kind. A host wait is never
		// orphaned, so it needs no form here.
		until := "it is " + w.until
		reset := "--on PREDECESSOR_ENTRY --owns PATH|--reason TEXT --until accepted|done|released"
		if w.onKind == api.TeamQueueWaitOnRelease {
			until = "its matrix departs"
			if w.until == api.TeamQueueWaitSettled {
				until = "it settles"
			}
			reset = "--on RELEASE_JOB --reason TEXT --until departed|settled [--resume handler]"
		}
		text := fmt.Sprintf("Queue entry %s (item %s) waits on %s for %s until %s, but %s, so that condition cannot be met. "+
			"Decide: clear the wait with tt team queue wait clear --task %s --entry %s --on %s and tell the waiting lead, "+
			"or clear it and set a new one with tt team queue wait set --task %s --entry %s %s. "+
			"The waiting lead was not told and nothing was released.",
			w.entry, w.item, w.onText(), w.waitedFor(), until, reason, w.task, w.entry, w.onEntry, w.task, w.entry, reset)
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
// member done, idle, finished silent or provider-blocked and none waiting for
// or holding the matrix host (the stall check's member rule,
// team_queue_stall.go, with its matrix wait rule in matrixWaitCounts). A
// provider-blocked member makes no progress.
func (s *Store) queueWaitTeamIdle(ctx context.Context, w queueWaitRow, now time.Time) (bool, error) {
	const members = `SELECT b.agent_id FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id
 WHERE b.item_task_id=? AND b.item_id=? AND a.role<>? AND a.status NOT IN ('closed','exited')`
	var posted int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE task_id=? AND seq>? AND from_agent IN (`+members+`)`,
		w.task, w.noticeSeq, w.task, w.item, api.AgentRoleDatabaseHandler).Scan(&posted); err != nil || posted > 0 {
		return false, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.status,COALESCE(x.state,''),COALESCE(x.payload,'') FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id
 LEFT JOIN agent_activity x ON x.agent_id=b.agent_id AND x.run_id=b.run_id
 WHERE b.item_task_id=? AND b.item_id=? AND a.role<>? AND a.status NOT IN ('closed','exited')`, w.task, w.item, api.AgentRoleDatabaseHandler)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	idle := true
	for rows.Next() {
		var status, state, payload string
		if err := rows.Scan(&status, &state, &payload); err != nil {
			return false, err
		}
		if status != api.AgentDone && state != "idle" && state != "finished_silent" && state != "provider_blocked" {
			idle = false
		}
		// A member's verification run waiting for or holding the matrix
		// host is the team's open work, whatever the member's own state. A
		// retired member's saved wait is not, and the bound runs from the
		// earlier of since and the report's observedAt (matrixWaitCounts).
		if strings.Contains(payload, `"matrixWait"`) {
			var activity api.AgentActivity
			if json.Unmarshal([]byte(payload), &activity) == nil && matrixWaitCounts(status, &activity, now) {
				idle = false
			}
		}
	}
	return idle, rows.Err()
}

// queueWaitReportTo is who an overdue report goes to: the primary handler
// and the newest live owner helper, either of which may be absent.
func queueWaitReportTo(ctx context.Context, tx *sql.Tx, task api.Task) (handler, helper api.Agent, err error) {
	if handler, err = queueWaitHandler(ctx, tx, task); err != nil {
		return handler, helper, err
	}
	helper, err = scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?,?) ORDER BY created_at DESC,id DESC LIMIT 1`, task.ID, api.AgentRoleOwnerHelper, api.AgentClosed, api.AgentExited, api.AgentRetired))
	if errors.Is(err, sql.ErrNoRows) {
		helper, err = api.Agent{}, nil
	}
	return handler, helper, err
}

// postQueueWaitReport posts one report to the owner helper and the primary
// handler and returns their message numbers, both 0 when neither exists.
func (s *Store) postQueueWaitReport(ctx context.Context, tx *sql.Tx, task api.Task, subject, text string, refs map[string]string) (helperSeq, handlerSeq int64, err error) {
	handler, helper, err := queueWaitReportTo(ctx, tx, task)
	if err != nil {
		return 0, 0, err
	}
	if helper.ID != "" {
		if helperSeq, err = s.postQueueWaitNotice(ctx, tx, task, helper, subject, text, refs); err != nil {
			return 0, 0, err
		}
	}
	if handler.ID != "" {
		if handlerSeq, err = s.postQueueWaitNotice(ctx, tx, task, handler, subject, text, refs); err != nil {
			return 0, 0, err
		}
	}
	return helperSeq, handlerSeq, nil
}

// queueWaitIdle follows a met automatic wait: a team that shows activity
// ends it as resumed; a team idle for the bound is reported once to the
// owner helper and the primary handler. With neither to tell, the wait
// stays met. A gated wait is judged by queueWaitGateDue instead.
func (s *Store) queueWaitIdle(ctx context.Context, w queueWaitRow, now time.Time) (*QueueWaitStep, error) {
	idle, err := s.queueWaitTeamIdle(ctx, w, now)
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
		commit := w.metCommit
		if commit == "" {
			commit = queueWaitNoCommit
		}
		held := "Paths free"
		if w.pathless() {
			held = "Paused for"
		}
		text := fmt.Sprintf("Queue entry %s (item %s) was told in message %d at %s that its wait on %s was met (%s, commit %s), and its team has been idle for %d minutes or more since. "+
			"%s: %s. Action: wake or message the waiting lead, or find out why the team has not resumed. Nothing was restarted or signalled.",
			w.entry, w.item, w.noticeSeq, w.metAt, w.onText(), w.until, commit, int(bound/time.Minute), held, w.waitedFor())
		refs := w.refs("queue_wait_overdue")
		refs["notice"] = strconv.FormatInt(w.noticeSeq, 10)
		helperSeq, handlerSeq, err := s.postQueueWaitReport(ctx, tx, task, queueWaitOverdueSubject, text, refs)
		if err != nil || (helperSeq == 0 && handlerSeq == 0) {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET state=?,helper_seq=?,handler_seq=?,closed_at=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
			api.TeamQueueWaitOverdue, helperSeq, handlerSeq, ts(now), w.task, w.entry, w.onEntry); err != nil {
			return false, err
		}
		step = &QueueWaitStep{TaskID: w.task, EntryID: w.entry, OnEntryID: w.onEntry, Action: "wait-overdue", MessageSeq: helperSeq}
		if helperSeq == 0 {
			step.MessageSeq = handlerSeq
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return step, nil
}

// queueWaitGateDue reports a wait that only the database handler's resume
// ends, once it is past its due time: a gated met wait at met_at plus the
// idle bound, a host wait at set_at plus queueWaitHostBound. The team's posts
// and work do not matter: what is judged is the handler's silence. The report
// goes once to the owner helper and the primary handler; the wait stays open
// and listed, and only the handler's wait clear closes it. With neither to
// tell, nothing is written and the next tick tries again.
func (s *Store) queueWaitGateDue(ctx context.Context, w queueWaitRow, now time.Time, tellers func(string) (bool, bool, error)) (*QueueWaitStep, error) {
	if w.helperSeq != 0 || w.handlerSeq != 0 {
		return nil, nil // already reported
	}
	host := w.onKind == api.TeamQueueWaitOnHost
	since, bound := parseTS(w.metAt), queueWaitIdleBound()
	if host {
		since, bound = parseTS(w.setAt), queueWaitHostBound
	}
	if now.Sub(since) < bound {
		return nil, nil
	}
	// With no one to tell, nothing is locked or written.
	if handler, helper, err := tellers(w.task); err != nil || (!handler && !helper) {
		return nil, err
	}
	var step *QueueWaitStep
	err := s.queueWaitTx(ctx, w, func(tx *sql.Tx, task api.Task) (bool, error) {
		var helperSeq, handlerSeq int64
		if err := tx.QueryRowContext(ctx, `SELECT helper_seq,handler_seq FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, w.task, w.entry, w.onEntry).Scan(&helperSeq, &handlerSeq); err != nil || helperSeq != 0 || handlerSeq != 0 {
			return false, err
		}
		held := "Paths free"
		if w.pathless() {
			held = "Paused for"
		}
		minutes := int(bound / time.Minute)
		text := fmt.Sprintf("Queue entry %s (item %s) has waited on host admission since %s, %d minutes or more, and the wait is still open: the database handler has not resumed the team or cleared the wait. ",
			w.entry, w.item, w.setAt, minutes)
		refs := w.refs("queue_wait_gate")
		if !host {
			commit := w.metCommit
			if commit == "" {
				commit = queueWaitNoCommit
			}
			text = fmt.Sprintf("Queue entry %s (item %s) was told in message %d at %s that its wait on %s was met (%s, commit %s). %d minutes or more later the wait is still open: the database handler has not resumed the team or cleared the wait. ",
				w.entry, w.item, w.noticeSeq, w.metAt, w.onText(), w.until, commit, minutes)
			refs["notice"] = strconv.FormatInt(w.noticeSeq, 10)
		}
		text += fmt.Sprintf("%s: %s. Action: this wait needs the database handler's explicit resume: run the gate's checks and send the resume to the waiting lead, or clear the wait "+
			"with tt team queue wait clear --task %s --entry %s --on %s. Whatever the team posts, only that clear ends this wait. Nothing was restarted or signalled.",
			held, w.waitedFor(), w.task, w.entry, w.onEntry)
		refs["resume"] = api.TeamQueueWaitResumeHandler
		helperSeq, handlerSeq, err := s.postQueueWaitReport(ctx, tx, task, queueWaitGateSubject, text, refs)
		if err != nil || (helperSeq == 0 && handlerSeq == 0) {
			return false, err
		}
		// The wait stays open: the sequences only record the report.
		if _, err := tx.ExecContext(ctx, `UPDATE team_queue_waits SET helper_seq=?,handler_seq=? WHERE task_id=? AND entry_id=? AND on_entry_id=?`,
			helperSeq, handlerSeq, w.task, w.entry, w.onEntry); err != nil {
			return false, err
		}
		step = &QueueWaitStep{TaskID: w.task, EntryID: w.entry, OnEntryID: w.onEntry, Action: "wait-overdue", MessageSeq: helperSeq}
		if helperSeq == 0 {
			step.MessageSeq = handlerSeq
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return step, nil
}
