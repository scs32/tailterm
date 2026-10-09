package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The cleanup of rows stored before the single-copy forms of retention.go.
//
// It runs as the last migration step, in four steps of fixed order, each
// walking its table by rowid in bounded transactions with its cursor and
// counters in storage_migrations, so an interrupted open resumes. A row is
// rewritten only when the rewrite is proved lossless for that row; any other
// row is left as it is and counted. No step touches release_action_receipts,
// a column a report reads, or the size of the file: freed pages go to the
// free list.

const (
	// retentionBatchRows is how many rows one transaction examines.
	retentionBatchRows = 500
	// retentionCheckpointBatches is how often the write-ahead log is measured
	// and truncated.
	retentionCheckpointBatches = 20
)

// errRetentionStopped is returned when a test hook stops the cleanup.
var errRetentionStopped = errors.New("storage cleanup stopped")

// retentionRun is how one run of the cleanup is bounded. The zero value is
// what an open uses. stop, set by a test, is asked after each committed batch
// whether to stop there.
type retentionRun struct {
	batchRows int
	stop      func(step string, batch int) bool
}

// retentionCounts is what one batch adds to its step's ledger row. Bytes are
// those of the columns a converted row held before and holds after.
type retentionCounts struct {
	examined, converted, left, bytesBefore, bytesAfter int64
}

// retentionStep is one cleanup step. batch examines the rows above the cursor
// and returns the last rowid it examined; zero rows means the step is done.
type retentionStep struct {
	name  string
	ready func(context.Context, *sql.DB) (bool, error)
	batch func(ctx context.Context, tx *sql.Tx, after int64, limit int) (last int64, rows int, counts retentionCounts, err error)
}

var retentionSteps = []retentionStep{
	{"revisions", retentionHasTables("usage_turn_revisions", "usage_turns"), retentionRevisionsBatch},
	{"turn-payload", retentionHasTables("usage_turns"), retentionTurnPayloadBatch},
	{"receipt-batch", retentionHasTables("usage_receipts"), retentionReceiptBatch},
	{"queue-launch", retentionQueueReady, retentionQueueLaunchBatch},
}

func retentionTableExists(ctx context.Context, q queryRower, table string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	return n > 0, err
}

func retentionHasTables(tables ...string) func(context.Context, *sql.DB) (bool, error) {
	return func(ctx context.Context, db *sql.DB) (bool, error) {
		for _, table := range tables {
			if ok, err := retentionTableExists(ctx, db, table); err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}
}

// retentionQueueReady reports whether queue request results can name the
// digest of a launch plan taken out of them.
func retentionQueueReady(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('team_queue_requests') WHERE name='launch_digest'`).Scan(&n)
	return n > 0, err
}

// retentionWALSize is the size of the database's write-ahead log file, zero
// for a database without one.
func retentionWALSize(ctx context.Context, db *sql.DB) int64 {
	rows, err := db.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return 0
	}
	file := ""
	for rows.Next() {
		var seq int
		var name, path string
		if rows.Scan(&seq, &name, &path) == nil && name == "main" {
			file = path
		}
	}
	rows.Close()
	if file == "" {
		return 0
	}
	info, err := os.Stat(file + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}

// retentionCheckpoint records the write-ahead log's size and truncates it.
func retentionCheckpoint(ctx context.Context, db *sql.DB, peak *int64) error {
	if size := retentionWALSize(ctx, db); size > *peak {
		*peak = size
	}
	rows, err := db.QueryContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err != nil {
		return err
	}
	return rows.Close()
}

// runRetentionCleanup runs every step that is not finished, in order.
func runRetentionCleanup(db *sql.DB, run retentionRun) error {
	ctx := context.Background()
	if run.batchRows <= 0 {
		run.batchRows = retentionBatchRows
	}
	for _, step := range retentionSteps {
		var state string
		var cursor, peak int64
		err := db.QueryRowContext(ctx, `SELECT state,cursor,wal_peak_bytes FROM storage_migrations WHERE step=?`, step.name).Scan(&state, &cursor, &peak)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if state == "done" || state == "skipped" {
			continue
		}
		if state == "" {
			ready, err := step.ready(ctx, db)
			if err != nil {
				return err
			}
			now := ts(time.Now())
			if !ready {
				// The form this step converts to does not exist here.
				if _, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO storage_migrations(step,state,started_at,finished_at) VALUES(?,'skipped',?,?)`, step.name, now, now); err != nil {
					return err
				}
				continue
			}
			var free int64
			if err = db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
				return err
			}
			if _, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO storage_migrations(step,state,freelist_before,started_at) VALUES(?,'running',?,?)`, step.name, free, now); err != nil {
				return err
			}
		}
		var total retentionCounts
		for batches := 1; ; batches++ {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			last, rows, counts, err := step.batch(ctx, tx, cursor, run.batchRows)
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("storage cleanup %s: %w", step.name, err)
			}
			if rows == 0 {
				var free int64
				if err = tx.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err == nil {
					_, err = tx.ExecContext(ctx, `UPDATE storage_migrations SET state='done',freelist_after=?,wal_peak_bytes=max(wal_peak_bytes,?),finished_at=? WHERE step=?`, free, peak, ts(time.Now()), step.name)
				}
				if err != nil {
					tx.Rollback()
					return err
				}
				if err = tx.Commit(); err != nil {
					return err
				}
				break
			}
			cursor = last
			if _, err = tx.ExecContext(ctx, `UPDATE storage_migrations SET cursor=?,examined=examined+?,converted=converted+?,"left"="left"+?,bytes_before=bytes_before+?,bytes_after=bytes_after+?,wal_peak_bytes=max(wal_peak_bytes,?) WHERE step=?`,
				cursor, counts.examined, counts.converted, counts.left, counts.bytesBefore, counts.bytesAfter, peak, step.name); err != nil {
				tx.Rollback()
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			total.examined += counts.examined
			total.converted += counts.converted
			total.left += counts.left
			if size := retentionWALSize(ctx, db); size > peak {
				peak = size
			}
			if batches%retentionCheckpointBatches == 0 {
				if err = retentionCheckpoint(ctx, db, &peak); err != nil {
					return err
				}
			}
			if run.stop != nil && run.stop(step.name, batches) {
				return errRetentionStopped
			}
		}
		if err = retentionCheckpoint(ctx, db, &peak); err != nil {
			return err
		}
		if total.examined > 0 {
			log.Printf("storage cleanup %s: examined %d, converted %d, left %d", step.name, total.examined, total.converted, total.left)
		}
	}
	return nil
}

// retentionRange returns the last rowid and the number of rows of the next
// batch of a table above a cursor.
func retentionRange(ctx context.Context, tx *sql.Tx, table string, after int64, limit int) (int64, int, error) {
	var last sql.NullInt64
	var rows int
	err := tx.QueryRowContext(ctx, `SELECT max(rowid),count(*) FROM (SELECT rowid FROM `+table+` WHERE rowid>? ORDER BY rowid LIMIT ?)`, after, limit).Scan(&last, &rows)
	return last.Int64, rows, err
}

// retentionRevisionIsCurrent selects the revision rows that usage_turns holds
// as well: same key, same revision, the same bytes in both documents.
const retentionRevisionIsCurrent = `rowid>? AND rowid<=? AND EXISTS (SELECT 1 FROM usage_turns t WHERE t.task_id=usage_turn_revisions.task_id AND t.agent_id=usage_turn_revisions.agent_id AND t.run_id=usage_turn_revisions.run_id AND t.request_id=usage_turn_revisions.request_id
 AND t.revision=usage_turn_revisions.revision AND t.payload=usage_turn_revisions.payload AND t.projection=usage_turn_revisions.projection)`

// retentionRevisionsBatch deletes the copies of current revisions. Superseded
// revisions are the history and are left.
func retentionRevisionsBatch(ctx context.Context, tx *sql.Tx, after int64, limit int) (int64, int, retentionCounts, error) {
	var counts retentionCounts
	last, rows, err := retentionRange(ctx, tx, "usage_turn_revisions", after, limit)
	if err != nil || rows == 0 {
		return 0, 0, counts, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(length(CAST(payload AS BLOB))+length(CAST(projection AS BLOB))),0) FROM usage_turn_revisions WHERE `+retentionRevisionIsCurrent, after, last).Scan(&counts.converted, &counts.bytesBefore); err != nil {
		return 0, 0, counts, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM usage_turn_revisions WHERE `+retentionRevisionIsCurrent, after, last)
	if err != nil {
		return 0, 0, counts, err
	}
	if deleted, err := result.RowsAffected(); err != nil || deleted != counts.converted {
		return 0, 0, counts, errors.New("deleted revisions differ from the rows counted")
	}
	counts.examined = int64(rows)
	counts.left = counts.examined - counts.converted
	return last, rows, counts, nil
}

// retentionTurnPayloadBatch empties the payload of a turn whose projection
// holds the same bytes as its turn member. Only payload is written.
func retentionTurnPayloadBatch(ctx context.Context, tx *sql.Tx, after int64, limit int) (int64, int, retentionCounts, error) {
	var counts retentionCounts
	rows, err := tx.QueryContext(ctx, `SELECT rowid,payload,projection FROM usage_turns WHERE rowid>? ORDER BY rowid LIMIT ?`, after, limit)
	if err != nil {
		return 0, 0, counts, err
	}
	type turn struct {
		rowid int64
		size  int64
	}
	var same []turn
	last, n := after, 0
	for rows.Next() {
		var rowid int64
		var payload, projection []byte
		if err = rows.Scan(&rowid, &payload, &projection); err != nil {
			rows.Close()
			return 0, 0, counts, err
		}
		last = rowid
		n++
		if len(payload) == 0 {
			continue // already stored once
		}
		var stored struct {
			Turn json.RawMessage `json:"turn"`
		}
		if json.Unmarshal(projection, &stored) != nil || !bytes.Equal(stored.Turn, payload) {
			counts.left++
			continue
		}
		same = append(same, turn{rowid, int64(len(payload))})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, counts, err
	}
	for _, t := range same {
		if _, err = tx.ExecContext(ctx, `UPDATE usage_turns SET payload='' WHERE rowid=?`, t.rowid); err != nil {
			return 0, 0, counts, err
		}
		counts.converted++
		counts.bytesBefore += t.size
	}
	counts.examined = int64(n)
	return last, n, counts, nil
}

// retentionReceiptBatch replaces a stored upload batch with its hash. The
// receipt and the key are not written.
func retentionReceiptBatch(ctx context.Context, tx *sql.Tx, after int64, limit int) (int64, int, retentionCounts, error) {
	var counts retentionCounts
	rows, err := tx.QueryContext(ctx, `SELECT rowid,payload FROM usage_receipts WHERE rowid>? ORDER BY rowid LIMIT ?`, after, limit)
	if err != nil {
		return 0, 0, counts, err
	}
	type receipt struct {
		rowid int64
		size  int64
		hash  string
	}
	var raw []receipt
	last, n := after, 0
	for rows.Next() {
		var rowid int64
		var payload []byte
		if err = rows.Scan(&rowid, &payload); err != nil {
			rows.Close()
			return 0, 0, counts, err
		}
		last = rowid
		n++
		if bytes.HasPrefix(payload, []byte(usageReceiptHashPrefix)) {
			continue // already a hash
		}
		raw = append(raw, receipt{rowid, int64(len(payload)), usageBatchHash(payload)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, counts, err
	}
	for _, r := range raw {
		if _, err = tx.ExecContext(ctx, `UPDATE usage_receipts SET payload=? WHERE rowid=?`, r.hash, r.rowid); err != nil {
			return 0, 0, counts, err
		}
		counts.converted++
		counts.bytesBefore += r.size
		counts.bytesAfter += int64(len(r.hash))
	}
	counts.examined = int64(n)
	return last, n, counts, nil
}

// queueResultLaunch is the launch member of a stored queue request result.
func queueResultLaunch(result []byte) (api.TeamQueueEntry, bool, error) {
	var e api.TeamQueueEntry
	if !bytes.Contains(result, []byte(`"launch"`)) {
		return e, false, nil
	}
	if err := json.Unmarshal(result, &e); err != nil {
		return e, false, err
	}
	return e, len(e.LaunchJSON) > 0 && string(e.LaunchJSON) != "null", nil
}

// retentionQueueLaunchBatch takes the launch plan out of a queue request
// result that still holds one, storing exactly what a new request stores. A
// result that does not re-encode to its stored bytes is left. Results are
// read one at a time: a plan is the largest document the cleanup handles.
func retentionQueueLaunchBatch(ctx context.Context, tx *sql.Tx, after int64, limit int) (int64, int, retentionCounts, error) {
	var counts retentionCounts
	rows, err := tx.QueryContext(ctx, `SELECT rowid FROM team_queue_requests WHERE rowid>? ORDER BY rowid LIMIT ?`, after, limit)
	if err != nil {
		return 0, 0, counts, err
	}
	var ids []int64
	for rows.Next() {
		var rowid int64
		if err = rows.Scan(&rowid); err != nil {
			rows.Close()
			return 0, 0, counts, err
		}
		ids = append(ids, rowid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) == 0 {
		return 0, 0, counts, err
	}
	for _, rowid := range ids {
		var result []byte
		var digest string
		if err = tx.QueryRowContext(ctx, `SELECT result_json,launch_digest FROM team_queue_requests WHERE rowid=?`, rowid).Scan(&result, &digest); err != nil {
			return 0, 0, counts, err
		}
		if digest != "" {
			continue // already stored without its plan
		}
		e, has, err := queueResultLaunch(result)
		if err != nil {
			counts.left++
			continue
		}
		if !has {
			continue // never held a plan
		}
		if again, err := json.Marshal(e); err != nil || !bytes.Equal(again, result) {
			counts.left++
			continue
		}
		compact, launchDigest, err := compactQueueResult(e)
		if err != nil || launchDigest == "" {
			counts.left++
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE team_queue_requests SET result_json=?,launch_digest=? WHERE rowid=?`, compact, launchDigest, rowid); err != nil {
			return 0, 0, counts, err
		}
		counts.converted++
		counts.bytesBefore += int64(len(result))
		counts.bytesAfter += int64(len(compact) + len(launchDigest))
	}
	counts.examined = int64(len(ids))
	return ids[len(ids)-1], len(ids), counts, nil
}

// The usage digest: a read-only proof that a database reports the same usage,
// budgets, warnings and audit export as another. It prints hashes, counts,
// sizes and times only.

// ErrUsageDigestWAL refuses a database whose write-ahead log holds pages the
// read-only open would not see.
var ErrUsageDigestWAL = errors.New("the database has a write-ahead log that is not empty")

// usageDigestWholeTables are hashed over every column in rowid order.
var usageDigestWholeTables = []string{
	"usage_item_shares", "usage_turn_totals", "usage_spans", "usage_runs",
	"usage_price_revisions", "usage_price_receipts", "usage_budget_warnings", "usage_entry_warnings",
	"usage_warning_settings", "usage_budgets", "usage_provider_readings", "usage_estimate_defaults",
	"work_item_estimates", "team_queue_budget_holds", "team_queue_entries", "release_jobs",
	"release_action_receipts", "release_check_lists",
}

// usageDigestKeptColumns are the tables the cleanup rewrites, hashed over the
// columns and rows it must keep.
var usageDigestKeptColumns = []struct{ name, table, query string }{
	{"usage_turns", "usage_turns", `SELECT task_id,agent_id,run_id,request_id,revision,at,projection FROM usage_turns ORDER BY rowid`},
	{"usage_turn_revisions.superseded", "usage_turn_revisions", `SELECT r.* FROM usage_turn_revisions r WHERE NOT EXISTS (SELECT 1 FROM usage_turns t WHERE t.task_id=r.task_id AND t.agent_id=r.agent_id AND t.run_id=r.run_id AND t.request_id=r.request_id AND t.revision=r.revision) ORDER BY r.rowid`},
	{"usage_receipts", "usage_receipts", `SELECT task_id,request_id,agent_id,run_id,receipt FROM usage_receipts ORDER BY rowid`},
	{"team_queue_requests", "team_queue_requests", `SELECT task_id,request_id,payload_hash FROM team_queue_requests ORDER BY rowid`},
}

// digestValue adds one column value to a hash, with its type and length, so
// neighbouring values cannot run together.
func digestValue(h hash.Hash, value any) {
	var n [9]byte
	text := func(kind byte, b []byte) {
		n[0] = kind
		binary.BigEndian.PutUint64(n[1:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	switch v := value.(type) {
	case nil:
		h.Write([]byte{'n'})
	case int64:
		n[0] = 'i'
		binary.BigEndian.PutUint64(n[1:], uint64(v))
		h.Write(n[:])
	case float64:
		n[0] = 'f'
		binary.BigEndian.PutUint64(n[1:], math.Float64bits(v))
		h.Write(n[:])
	case bool:
		if v {
			h.Write([]byte{'i', 0, 0, 0, 0, 0, 0, 0, 1})
		} else {
			h.Write([]byte{'i', 0, 0, 0, 0, 0, 0, 0, 0})
		}
	case string:
		text('s', []byte(v))
	case []byte:
		text('b', v)
	case time.Time:
		text('t', []byte(v.UTC().Format(time.RFC3339Nano)))
	default:
		text('?', []byte(fmt.Sprint(v)))
	}
}

// digestRows hashes every row a query returns into h and counts them.
func digestRows(ctx context.Context, q queryRower, h hash.Hash, query string, args ...any) (int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	values := make([]any, len(cols))
	dest := make([]any, len(cols))
	for i := range values {
		dest[i] = &values[i]
	}
	var count int64
	for rows.Next() {
		if err = rows.Scan(dest...); err != nil {
			return count, err
		}
		for _, value := range values {
			digestValue(h, value)
		}
		count++
	}
	return count, rows.Err()
}

func digestHex(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func digestJSON(h hash.Hash, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	digestValue(h, b)
	return nil
}

// UsageDigest opens the database at path read-only, without creating,
// migrating or locking anything, and writes its digest to w. The clock is
// pinned to the newest stored turn, so two runs on one file write the same
// bytes.
func UsageDigest(ctx context.Context, path string, w io.Writer) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if wal, err := os.Stat(path + "-wal"); err == nil && wal.Size() > 0 {
		return ErrUsageDigestWAL
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var pages, free int64
	if err = db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return err
	}
	if err = db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return err
	}
	fmt.Fprintf(w, "file size=%d pages=%d free=%d\n", info.Size(), pages, free)

	for _, table := range usageDigestWholeTables {
		if err = digestTable(ctx, db, w, table, table, `SELECT * FROM `+table+` ORDER BY rowid`); err != nil {
			return err
		}
	}
	for _, kept := range usageDigestKeptColumns {
		if err = digestTable(ctx, db, w, kept.name, kept.table, kept.query); err != nil {
			return err
		}
	}
	if err = digestForms(ctx, db, w); err != nil {
		return err
	}
	if err = digestSteps(ctx, db, w); err != nil {
		return err
	}

	var newest sql.NullString
	if ok, err := retentionTableExists(ctx, db, "usage_turns"); err != nil {
		return err
	} else if ok {
		if err = db.QueryRowContext(ctx, `SELECT max(at) FROM usage_turns`).Scan(&newest); err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintf(w, "export omits=%s\n", usageDigestClockOmitted); err != nil {
		return err
	}
	pinned := parseTS(newest.String)
	s := &Store{MaxAgents: api.MaxAgentsPerTask, db: db, waiters: map[string]chan struct{}{}, now: func() time.Time { return pinned }}
	projects, err := digestStrings(ctx, db, `SELECT id FROM tasks ORDER BY id`)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if err = s.digestProject(ctx, w, project); err != nil {
			return err
		}
	}
	return nil
}

func digestStrings(ctx context.Context, q queryRower, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// digestTable writes one table line: the row count and the hash of the rows a
// query selects, or that the table is not in this database.
func digestTable(ctx context.Context, db *sql.DB, w io.Writer, name, table, query string) error {
	if ok, err := retentionTableExists(ctx, db, table); err != nil {
		return err
	} else if !ok {
		_, err = fmt.Fprintf(w, "table %s absent\n", name)
		return err
	}
	h := sha256.New()
	rows, err := digestRows(ctx, db, h, query)
	if err != nil {
		return fmt.Errorf("digest of table %s failed", name)
	}
	_, err = fmt.Fprintf(w, "table %s rows=%d sha256=%s\n", name, rows, digestHex(h))
	return err
}

// digestForms writes, per kind, how many rows are stored in the single-copy
// form and how many in the form before it.
func digestForms(ctx context.Context, db *sql.DB, w io.Writer) error {
	count := func(name, table, query string) error {
		if ok, err := retentionTableExists(ctx, db, table); err != nil || !ok {
			return err
		}
		var newForm, oldForm sql.NullInt64
		if err := db.QueryRowContext(ctx, query).Scan(&newForm, &oldForm); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, "form %s new=%d old=%d\n", name, newForm.Int64, oldForm.Int64)
		return err
	}
	if err := count("usage_turns", "usage_turns", `SELECT sum(payload=''),sum(payload<>'') FROM usage_turns`); err != nil {
		return err
	}
	if err := count("usage_turn_revisions", "usage_turn_revisions", `SELECT sum(NOT current),sum(current) FROM (SELECT EXISTS (SELECT 1 FROM usage_turns t WHERE t.task_id=r.task_id AND t.agent_id=r.agent_id AND t.run_id=r.run_id AND t.request_id=r.request_id AND t.revision=r.revision) AS current FROM usage_turn_revisions r)`); err != nil {
		return err
	}
	if err := count("usage_receipts", "usage_receipts", `SELECT sum(substr(payload,1,7)='sha256:'),sum(substr(payload,1,7)<>'sha256:') FROM usage_receipts`); err != nil {
		return err
	}
	scan := func(name, table, query string, form func(document []byte, digest string) (bool, bool)) error {
		if ok, err := retentionTableExists(ctx, db, table); err != nil || !ok {
			return err
		}
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		var newForm, oldForm, neither int64
		for rows.Next() {
			var document []byte
			var digest string
			if err = rows.Scan(&document, &digest); err != nil {
				return err
			}
			switch isNew, isOld := form(document, digest); {
			case isNew:
				newForm++
			case isOld:
				oldForm++
			default:
				neither++
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "form %s new=%d old=%d neither=%d\n", name, newForm, oldForm, neither)
		return err
	}
	if err := scan("release_action_receipts", "release_action_receipts", `SELECT record_json,'' FROM release_action_receipts ORDER BY rowid`, func(document []byte, _ string) (bool, bool) {
		type checks struct {
			Checks []json.RawMessage `json:"checks"`
		}
		var record struct {
			Plan                   checks           `json:"plan"`
			IntegratedPlan         *checks          `json:"integratedPlan"`
			IntegratedVerification *checks          `json:"integratedVerification"`
			CheckLists             *json.RawMessage `json:"checkLists"`
		}
		if json.Unmarshal(document, &record) != nil {
			return false, false
		}
		inline := len(record.Plan.Checks) > 0 || (record.IntegratedPlan != nil && len(record.IntegratedPlan.Checks) > 0) || (record.IntegratedVerification != nil && len(record.IntegratedVerification.Checks) > 0)
		return record.CheckLists != nil, inline
	}); err != nil {
		return err
	}
	queueQuery := `SELECT result_json,launch_digest FROM team_queue_requests ORDER BY rowid`
	if ok, err := retentionQueueReady(ctx, db); err != nil {
		return err
	} else if !ok {
		queueQuery = `SELECT result_json,'' FROM team_queue_requests ORDER BY rowid`
	}
	return scan("team_queue_requests", "team_queue_requests", queueQuery, func(document []byte, digest string) (bool, bool) {
		if digest != "" {
			return true, false
		}
		_, has, err := queueResultLaunch(document)
		return false, err != nil || has
	})
}

// digestSteps writes the cleanup's ledger rows. A database no cleanup has
// opened has no ledger and no step lines.
func digestSteps(ctx context.Context, db *sql.DB, w io.Writer) error {
	if ok, err := retentionTableExists(ctx, db, "storage_migrations"); err != nil || !ok {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT step,state,cursor,examined,converted,"left",bytes_before,bytes_after,wal_peak_bytes,freelist_before,freelist_after,started_at,finished_at FROM storage_migrations ORDER BY CASE step WHEN 'revisions' THEN 1 WHEN 'turn-payload' THEN 2 WHEN 'receipt-batch' THEN 3 WHEN 'queue-launch' THEN 4 ELSE 5 END,step`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var step, state, started, finished string
		var cursor, examined, converted, left, before, after, wal, freeBefore, freeAfter int64
		if err = rows.Scan(&step, &state, &cursor, &examined, &converted, &left, &before, &after, &wal, &freeBefore, &freeAfter, &started, &finished); err != nil {
			return err
		}
		seconds := "-"
		if a, b := parseTS(started), parseTS(finished); !a.IsZero() && !b.IsZero() {
			seconds = fmt.Sprintf("%.3f", b.Sub(a).Seconds())
		}
		if state != "running" && state != "done" && state != "skipped" {
			state = "other"
		}
		if _, err = fmt.Fprintf(w, "step %s state=%s cursor=%d examined=%d converted=%d left=%d bytes_before=%d bytes_after=%d wal_peak=%d freelist_before=%d freelist_after=%d seconds=%s\n",
			retentionStepName(step), state, cursor, examined, converted, left, before, after, wal, freeBefore, freeAfter, seconds); err != nil {
			return err
		}
	}
	return rows.Err()
}

// retentionStepName prints a ledger step only when it is one of the cleanup's
// own names.
func retentionStepName(step string) string {
	for _, known := range retentionSteps {
		if known.name == step {
			return step
		}
	}
	return "other"
}

// Every ordinary open reconciles each work item's history and writes the time
// of that open to work_item_history_state.checked_at, so the export of one
// database differs between any two opens in that column alone. The digest's
// export hash leaves out that one column and nothing else: it reads that one
// stream by its other columns, and every other stream by the export's own
// query.
const (
	usageDigestClockStream  = "workItemHistoryState"
	usageDigestClockOmitted = "work_item_history_state.checked_at"
	usageDigestClockQuery   = `SELECT item_task_id,item_id,observed_current_revision,latest_materialized_revision,complete FROM work_item_history_state WHERE item_task_id=? ORDER BY item_id`
)

// digestExportQueries are the streams of the current audit export format.
func digestExportQueries(ctx context.Context, db *sql.DB) ([]exportQuery, error) {
	queries := append(append(append(append([]exportQuery(nil), exportQueries...), allocationIntentExportQueries...), projectPauseExportQueries...), queueExportQueries...)
	queries = append(queries, workItemEstimateExportQueries...)
	queries = append(queries, phaseSuccessorExportQueries[:2]...)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	legacy, err := hasLegacyPhaseSuccessorTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	if legacy {
		queries = append(queries, phaseSuccessorExportQueries[2:]...)
	}
	return queries, nil
}

// digestProject writes one project line: its turn count and the hashes of
// what the usage view, the token guardrails and the audit export read.
func (s *Store) digestProject(ctx context.Context, w io.Writer, project string) error {
	failed := func(what string) error { return fmt.Errorf("digest of a project's %s failed", what) }
	var turns int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM usage_turns WHERE task_id=?`, project).Scan(&turns); err != nil {
		return failed("turns")
	}
	report := sha256.New()
	if api.ValidID(project, "tsk") {
		usage, err := s.Usage(ctx, project, api.UsageQuery{})
		if err != nil || digestJSON(report, usage) != nil {
			return failed("usage report")
		}
	}
	budgets := sha256.New()
	items, err := digestStrings(ctx, s.db, `SELECT id FROM work_items WHERE task_id=? ORDER BY seq`, project)
	if err != nil {
		return failed("items")
	}
	for _, item := range items {
		budget, err := loadTokenBudget(ctx, s.db, project, item)
		if err != nil {
			return failed("budgets")
		}
		digestValue(budgets, item)
		if digestJSON(budgets, budget) != nil {
			return failed("budgets")
		}
	}
	warnings := sha256.New()
	if api.ValidID(project, "tsk") {
		read, err := readUsageWarnings(ctx, s.db, project)
		if err != nil || digestJSON(warnings, read) != nil {
			return failed("warnings")
		}
	}
	// A queue entry shows the budget of its item, read by the same loader.
	entries := sha256.New()
	rows, err := s.db.QueryContext(ctx, `SELECT id,item_id FROM team_queue_entries WHERE task_id=? ORDER BY id`, project)
	if err != nil {
		return failed("queue entries")
	}
	var queued [][2]string
	for rows.Next() {
		var entry [2]string
		if err = rows.Scan(&entry[0], &entry[1]); err != nil {
			rows.Close()
			return failed("queue entries")
		}
		queued = append(queued, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return failed("queue entries")
	}
	for _, entry := range queued {
		budget, err := loadTokenBudget(ctx, s.db, project, entry[1])
		if err != nil {
			return failed("queue entry budgets")
		}
		digestValue(entries, entry[0])
		if digestJSON(entries, budget) != nil {
			return failed("queue entry budgets")
		}
	}
	export := sha256.New()
	queries, err := digestExportQueries(ctx, s.db)
	if err != nil {
		return failed("audit export")
	}
	for _, q := range queries {
		digestValue(export, q.name)
		query := q.sql
		if q.name == usageDigestClockStream {
			query = usageDigestClockQuery
		}
		count, err := digestRows(ctx, s.db, export, query, q.args(project)...)
		if err != nil {
			return failed("audit export")
		}
		digestValue(export, count)
	}
	_, err = fmt.Fprintf(w, "project %s turns=%d report=%s budgets=%s warnings=%s entries=%s export=%s\n",
		retentionHash([]byte(project))[:16], turns, digestHex(report), digestHex(budgets), digestHex(warnings), digestHex(entries), digestHex(export))
	return err
}
