package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// teamQueueEntryColumns lists every column of team_queue_entries in table
// order. An item may have several entries, one per attempt, so the table has
// no uniqueness on the item; the indexes below keep attempts distinct and at
// most one of them live.
var teamQueueEntryColumns = []struct{ name, definition string }{
	{"id", "TEXT PRIMARY KEY"},
	{"task_id", "TEXT NOT NULL REFERENCES tasks(id)"},
	{"item_id", "TEXT NOT NULL REFERENCES work_items(id)"},
	{"item_revision", "INTEGER NOT NULL"},
	{"order_seq", "INTEGER NOT NULL"},
	{"template", "TEXT NOT NULL"},
	{"position", "INTEGER NOT NULL"},
	{"state", "TEXT NOT NULL CHECK(state IN ('queued','launching','running','finished','failed'))"},
	{"revision", "INTEGER NOT NULL DEFAULT 1"},
	{"host", "TEXT NOT NULL DEFAULT ''"},
	{"cwd", "TEXT NOT NULL DEFAULT ''"},
	{"pause_generation", "INTEGER NOT NULL DEFAULT 0"},
	{"launch_json", "BLOB NOT NULL DEFAULT ''"},
	{"close_json", "BLOB NOT NULL DEFAULT ''"},
	{"failure", "TEXT NOT NULL DEFAULT ''"},
	{"escalation_seq", "INTEGER NOT NULL DEFAULT 0"},
	{"created_at", "TEXT NOT NULL"},
	{"updated_at", "TEXT NOT NULL"},
	{"released_at", "TEXT NOT NULL DEFAULT ''"},
	{"repository", "TEXT NOT NULL DEFAULT ''"},
	{"ownership_json", "TEXT NOT NULL DEFAULT '[]'"},
	{"handler_id", "TEXT NOT NULL DEFAULT ''"},
	{"handler_run_id", "TEXT NOT NULL DEFAULT ''"},
	{"handler_lease_generation", "INTEGER NOT NULL DEFAULT 0"},
	{"base_commit", "TEXT NOT NULL DEFAULT ''"},
	{"integration_json", "TEXT NOT NULL DEFAULT ''"},
	{"acceptance_json", "TEXT NOT NULL DEFAULT ''"},
	{"serial", "INTEGER NOT NULL DEFAULT 0"},
	{"owner_integration_json", "TEXT NOT NULL DEFAULT ''"},
	{"attempt", "INTEGER NOT NULL DEFAULT 1"},
	{"retry_of", "TEXT NOT NULL DEFAULT ''"},
}

// teamQueueEntryColumnsSQL is the column list of a new team_queue_entries
// table; teamQueueEntryColumnNames is the same list for an explicit copy.
var teamQueueEntryColumnsSQL, teamQueueEntryColumnNames = func() (string, string) {
	var definitions, names []string
	for _, column := range teamQueueEntryColumns {
		definitions = append(definitions, column.name+" "+column.definition)
		names = append(names, column.name)
	}
	return strings.Join(definitions, ", "), strings.Join(names, ",")
}()

// teamQueueItemUnique is the inline constraint of the first queue, which
// allowed one entry per item, ever.
const teamQueueItemUnique = "UNIQUE(task_id,item_id)"

// rebuildTeamQueueEntries drops the one-entry-per-item constraint from an
// older table. SQLite cannot drop an inline constraint, so the rows are
// copied, column by column, into a table without it. A column this version
// does not know stops the migration instead of being dropped.
func rebuildTeamQueueEntries(tx *sql.Tx) error {
	var tableSQL string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='team_queue_entries'`).Scan(&tableSQL); err != nil {
		return err
	}
	if !strings.Contains(strings.Join(strings.Fields(tableSQL), ""), teamQueueItemUnique) {
		return nil
	}
	known := map[string]bool{}
	for _, column := range teamQueueEntryColumns {
		known[column.name] = true
	}
	rows, err := tx.Query(`SELECT name FROM pragma_table_info('team_queue_entries')`)
	if err != nil {
		return err
	}
	present := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !known[name] {
			rows.Close()
			return fmt.Errorf("team_queue_entries has unknown column %q; refusing to rebuild it", name)
		}
		present++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if present != len(teamQueueEntryColumns) {
		return fmt.Errorf("team_queue_entries has %d of %d columns; refusing to rebuild it", present, len(teamQueueEntryColumns))
	}
	var before, after int
	if err := tx.QueryRow(`SELECT count(*) FROM team_queue_entries`).Scan(&before); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS team_queue_entries_v2;
		CREATE TABLE team_queue_entries_v2 (` + teamQueueEntryColumnsSQL + `);
		INSERT INTO team_queue_entries_v2(` + teamQueueEntryColumnNames + `) SELECT ` + teamQueueEntryColumnNames + ` FROM team_queue_entries;`); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM team_queue_entries_v2`).Scan(&after); err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("team_queue_entries rebuild copied %d of %d rows", after, before)
	}
	_, err = tx.Exec(`DROP TABLE team_queue_entries;
		ALTER TABLE team_queue_entries_v2 RENAME TO team_queue_entries;`)
	return err
}

func migrateTeamQueue(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldTable, pendingTable int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='team_launch_reservations'`).Scan(&oldTable); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='team_launch_reservations_v2'`).Scan(&pendingTable); err != nil {
		return err
	}
	if pendingTable != 0 {
		if oldTable == 0 {
			if _, err := tx.Exec(`ALTER TABLE team_launch_reservations_v2 RENAME TO team_launch_reservations`); err != nil {
				return err
			}
		} else if _, err := tx.Exec(`DROP TABLE team_launch_reservations_v2`); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS team_queue_entries (` + teamQueueEntryColumnsSQL + `);
 CREATE TABLE IF NOT EXISTS team_queue_requests(task_id TEXT NOT NULL,request_id TEXT NOT NULL,payload_hash TEXT NOT NULL,result_json BLOB NOT NULL,PRIMARY KEY(task_id,request_id));
 CREATE TABLE IF NOT EXISTS team_launch_reservations(task_id TEXT NOT NULL REFERENCES tasks(id),entry_id TEXT NOT NULL DEFAULT '',item_id TEXT NOT NULL, token TEXT NOT NULL,
 pause_generation INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('reserved','launching','running')), created_at TEXT NOT NULL,PRIMARY KEY(task_id,entry_id));`)
	if err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('team_queue_entries') WHERE name='released_at'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		_, err = tx.Exec(`ALTER TABLE team_queue_entries ADD COLUMN released_at TEXT NOT NULL DEFAULT ''`)
	}
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"repository", "TEXT NOT NULL DEFAULT ''"},
		{"ownership_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"handler_id", "TEXT NOT NULL DEFAULT ''"},
		{"handler_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"handler_lease_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"base_commit", "TEXT NOT NULL DEFAULT ''"},
		{"integration_json", "TEXT NOT NULL DEFAULT ''"},
		{"acceptance_json", "TEXT NOT NULL DEFAULT ''"},
		{"serial", "INTEGER NOT NULL DEFAULT 0"},
		{"owner_integration_json", "TEXT NOT NULL DEFAULT ''"},
		{"attempt", "INTEGER NOT NULL DEFAULT 1"},
		{"retry_of", "TEXT NOT NULL DEFAULT ''"},
	} {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('team_queue_entries') WHERE name=?`, column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := tx.Exec("ALTER TABLE team_queue_entries ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	// Every column exists now, so the copy below is complete.
	if err := rebuildTeamQueueEntries(tx); err != nil {
		return err
	}
	// One row per attempt of an item, and at most one attempt that is live:
	// queued, launching, running, or failed and not yet released.
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS team_queue_order ON team_queue_entries(task_id,position);
		CREATE UNIQUE INDEX IF NOT EXISTS team_queue_item_attempt ON team_queue_entries(task_id,item_id,attempt);
		CREATE UNIQUE INDEX IF NOT EXISTS team_queue_item_live ON team_queue_entries(task_id,item_id) WHERE state IN ('queued','launching','running') OR (state='failed' AND released_at='');
		CREATE TABLE IF NOT EXISTS team_queue_rebinds(id TEXT PRIMARY KEY, task_id TEXT NOT NULL, entry_id TEXT NOT NULL, item_id TEXT NOT NULL,
			from_item_revision INTEGER NOT NULL, to_item_revision INTEGER NOT NULL, from_scope_revision INTEGER NOT NULL, to_scope_revision INTEGER NOT NULL,
			order_seq INTEGER NOT NULL, source_message_seq INTEGER NOT NULL,
			amended_agent TEXT NOT NULL DEFAULT '', amended_node TEXT NOT NULL DEFAULT '', amended_user TEXT NOT NULL DEFAULT '',
			approved_agent TEXT NOT NULL DEFAULT '', approved_run TEXT NOT NULL DEFAULT '', approved_node TEXT NOT NULL DEFAULT '', approved_user TEXT NOT NULL DEFAULT '',
			entry_state TEXT NOT NULL, created_at TEXT NOT NULL);
		CREATE INDEX IF NOT EXISTS team_queue_rebinds_entry ON team_queue_rebinds(task_id,entry_id,created_at);
		CREATE TABLE IF NOT EXISTS team_queue_rebind_bindings(rebind_id TEXT NOT NULL REFERENCES team_queue_rebinds(id), agent_id TEXT NOT NULL, run_id TEXT NOT NULL,
			from_item_revision INTEGER NOT NULL, to_item_revision INTEGER NOT NULL, context_digest TEXT NOT NULL, PRIMARY KEY(rebind_id,agent_id,run_id));
		CREATE INDEX IF NOT EXISTS team_queue_rebind_bindings_run ON team_queue_rebind_bindings(agent_id,run_id);`); err != nil {
		return err
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS team_queue_settings(task_id TEXT PRIMARY KEY REFERENCES tasks(id), concurrency_limit INTEGER NOT NULL DEFAULT 1 CHECK(concurrency_limit >= 0));
		CREATE TABLE IF NOT EXISTS team_host_policies(host TEXT PRIMARY KEY,version INTEGER NOT NULL,expires_at TEXT NOT NULL,max_sessions INTEGER NOT NULL,max_polling INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS team_host_usage(host TEXT PRIMARY KEY,limiter_domain TEXT NOT NULL,policy_version INTEGER NOT NULL DEFAULT 0,observed_at TEXT NOT NULL,relay_bindings INTEGER NOT NULL,complete INTEGER NOT NULL,source_digest TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS item_team_leads(task_id TEXT NOT NULL,item_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,revision INTEGER NOT NULL DEFAULT 1,state TEXT NOT NULL CHECK(state IN ('launching','running','closed')),PRIMARY KEY(task_id,item_id));
		DROP INDEX IF EXISTS team_queue_active;
		CREATE INDEX IF NOT EXISTS team_queue_active ON team_queue_entries(task_id,state) WHERE state IN ('launching','running');`)
	if err != nil {
		return err
	}
	// The first parallel queue allowed only 1 or 2. Zero now means no fixed
	// cap, so an older table is rebuilt with the wider check.
	var settingsSQL string
	if err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='team_queue_settings'`).Scan(&settingsSQL); err != nil {
		return err
	}
	if strings.Contains(settingsSQL, "BETWEEN 1 AND 2") {
		if _, err := tx.Exec(`CREATE TABLE team_queue_settings_v2(task_id TEXT PRIMARY KEY REFERENCES tasks(id), concurrency_limit INTEGER NOT NULL DEFAULT 1 CHECK(concurrency_limit >= 0));
			INSERT INTO team_queue_settings_v2(task_id,concurrency_limit) SELECT task_id,concurrency_limit FROM team_queue_settings;
			DROP TABLE team_queue_settings;
			ALTER TABLE team_queue_settings_v2 RENAME TO team_queue_settings;`); err != nil {
			return err
		}
	}
	for _, column := range []struct{ name, definition string }{
		{"min_free_disk_mib", "INTEGER NOT NULL DEFAULT 0"},
		{"limiter_domain", "TEXT NOT NULL DEFAULT ''"},
		{"max_relay_bindings", "INTEGER NOT NULL DEFAULT 0"},
		{"max_requests_per_minute", "INTEGER NOT NULL DEFAULT 0"},
		{"max_burst", "INTEGER NOT NULL DEFAULT 0"},
		{"headroom_percent", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('team_host_policies') WHERE name=?`, column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := tx.Exec("ALTER TABLE team_host_policies ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	for _, column := range []struct{ name, definition string }{
		{"policy_version", "INTEGER NOT NULL DEFAULT 0"},
		{"free_disk_mib", "INTEGER"},
	} {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('team_host_usage') WHERE name=?`, column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := tx.Exec("ALTER TABLE team_host_usage ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	var entryPK int
	rows, err := tx.Query(`PRAGMA table_info('team_launch_reservations')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "entry_id" {
			entryPK = pk
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if entryPK == 0 {
		_, err = tx.Exec(`CREATE TABLE team_launch_reservations_v2(task_id TEXT NOT NULL REFERENCES tasks(id),entry_id TEXT NOT NULL DEFAULT '',item_id TEXT NOT NULL,token TEXT NOT NULL,pause_generation INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('reserved','launching','running')),created_at TEXT NOT NULL,PRIMARY KEY(task_id,entry_id));
			INSERT INTO team_launch_reservations_v2 SELECT task_id,entry_id,item_id,token,pause_generation,state,created_at FROM team_launch_reservations;
			DROP TABLE team_launch_reservations;
			ALTER TABLE team_launch_reservations_v2 RENAME TO team_launch_reservations;`)
		if err != nil {
			return err
		}
	}
	var hostColumn int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('team_launch_reservations') WHERE name='host'`).Scan(&hostColumn); err != nil {
		return err
	}
	if hostColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE team_launch_reservations ADD COLUMN host TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// queueConcurrencyLimit reads a project's team queue limit. A project with no
// setting is the serial queue (1); 0 means no fixed cap, and N >= 2 is an
// owner ceiling. Parallel admission is otherwise governed by ownership,
// handler leases, host capacity and the disk reserve.
func queueConcurrencyLimit(ctx context.Context, q queryRower, task string) (int, error) {
	var limit int
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&limit)
	return limit, err
}

// queueParallel reports whether a limit runs teams beside each other, with
// item-scoped leads instead of the project lead slot.
func queueParallel(limit int) bool { return limit != 1 }

// queueSlotsFull reports whether a positive limit is reached. Zero has no cap.
func queueSlotsFull(limit, active int) bool { return limit > 0 && active >= limit }

// projectQueueParallel reads whether a project's team queue is parallel.
func projectQueueParallel(ctx context.Context, q queryRower, task string) (bool, error) {
	limit, err := queueConcurrencyLimit(ctx, q, task)
	return queueParallel(limit), err
}

// freeQueueHandler returns the first online database handler that no active
// entry leases, or a zero agent. Neither side of an open handler rotation
// takes a new lease.
func freeQueueHandler(ctx context.Context, q queryRower, task string, active []api.TeamQueueEntry) (api.Agent, error) {
	free, err := freeQueueHandlers(ctx, q, task, active)
	if err != nil || len(free) == 0 {
		return api.Agent{}, err
	}
	return free[0], nil
}

// freeQueueHandlers lists every free handler in lease order.
func freeQueueHandlers(ctx context.Context, q queryRower, task string, active []api.TeamQueueEntry) ([]api.Agent, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN ('retired','closed','exited')
 AND id NOT IN (SELECT old_agent_id FROM handler_rotations WHERE task_id=? AND state='prepared' UNION SELECT successor_agent_id FROM handler_rotations WHERE task_id=? AND state='prepared') ORDER BY created_at,id`, task, api.AgentRoleDatabaseHandler, task, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		if !a.Online {
			continue
		}
		inUse := false
		for _, entry := range active {
			if entry.HandlerID == a.ID {
				inUse = true
				break
			}
		}
		if !inUse {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// liveItemRuns counts the item-bound runs that are not yet closed and
// cleaned, which keep a failed entry from being released.
func liveItemRuns(ctx context.Context, q queryRower, task, item string) (int, error) {
	var live int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND (a.status<>'closed' OR a.cleanup_done=0)`, task, item).Scan(&live)
	return live, err
}

func validTeamQueueID(id string) bool {
	if !strings.HasPrefix(id, "tqe_") || len(id) != 20 {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
}

func validGitCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Releasing a reservation is an explicit owner action. Every known run of the
// item must have a durable close and cleanup receipt. An attempted spawn with
// no registration needs exact frozen identity and synchronized host absence
// proof; the hub rechecks that the agent is still absent in this transaction.
func queueReleaseSafe(ctx context.Context, tx *sql.Tx, task, item, entry, host string, launch json.RawMessage, proof *api.TeamQueueReleaseProof) error {
	var plan struct {
		Members []struct {
			Fields struct {
				AgentID string `json:"agentId"`
				Name    string `json:"name"`
			} `json:"fields"`
			State string `json:"state"`
			RunID string `json:"runId"`
		} `json:"members"`
	}
	if len(launch) != 0 && json.Unmarshal(launch, &plan) != nil {
		return api.ErrConflict
	}
	unresolved := map[string]api.TeamQueueReleaseMember{}
	if proof != nil {
		digest := sha256.Sum256(launch)
		if entry == "" || proof.TaskID != task || proof.EntryID != entry || proof.ItemID != item || proof.Host != host || proof.LaunchDigest != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("%w: queue release proof identity differs from frozen launch", api.ErrConflict)
		}
		for _, p := range proof.Members {
			if !api.ValidID(p.AgentID, "agt") || !validRunID(p.RunID) || p.Name == "" || unresolved[p.AgentID].AgentID != "" {
				return api.ErrInvalid
			}
			unresolved[p.AgentID] = p
		}
	}
	for _, m := range plan.Members {
		if m.State == "unstarted" {
			continue
		}
		if m.State != "started" && m.State != "uncertain" {
			return api.ErrConflict
		}
		var actualRun, status string
		var cleanup bool
		err := tx.QueryRowContext(ctx, `SELECT run_id,status,cleanup_done FROM agents WHERE task_id=? AND id=?`, task, m.Fields.AgentID).Scan(&actualRun, &status, &cleanup)
		if errors.Is(err, sql.ErrNoRows) && m.State == "uncertain" {
			p, ok := unresolved[m.Fields.AgentID]
			if !ok || p.RunID != m.RunID || p.Name != m.Fields.Name {
				return fmt.Errorf("%w: uncertain spawn lacks exact synchronized host absence proof", api.ErrConflict)
			}
			delete(unresolved, m.Fields.AgentID)
			continue
		}
		if err != nil || actualRun != m.RunID || status != api.AgentClosed || !cleanup {
			return fmt.Errorf("%w: attempted spawn lacks exact close and cleanup receipts", api.ErrConflict)
		}
	}
	if len(unresolved) != 0 {
		return fmt.Errorf("%w: release proof names an unexpected attempted run", api.ErrConflict)
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.status,a.cleanup_done FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=?`, task, item)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var cleanup bool
		if err := rows.Scan(&status, &cleanup); err != nil {
			return err
		}
		if status != api.AgentClosed || !cleanup {
			return fmt.Errorf("%w: item team still has live or uncleaned runs", api.ErrConflict)
		}
	}
	return rows.Err()
}

func recordedTeamOrder(ctx context.Context, tx *sql.Tx, task, item string, revision, seq int64) error {
	var linked int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM message_work_item_links WHERE item_task_id=? AND item_id=? AND message_seq=? AND message_task_id=? AND relationship='primary' AND item_revision<=?`, task, item, seq, task, revision).Scan(&linked)
	if err != nil {
		return err
	}
	if linked != 1 {
		return fmt.Errorf("%w: selected order is not recorded for this item", api.ErrConflict)
	}
	return nil
}

// The small-change lane (wi_f8d48780626165cc) is an explicit owner or
// helper choice for a bug whose fix fits a file, its test and a doc. Anything
// larger is requeued as Planned delivery.
const smallChangeMaxOwned = 3

// smallChangeOwnership refuses a small-change entry that does not declare a
// bounded ownership scope.
func smallChangeOwnership(ownership []string, serial bool) error {
	if serial || len(ownership) == 0 {
		return fmt.Errorf("%w: the small-change lane needs explicit ownership (--owns), not --serial", api.ErrConflict)
	}
	if len(ownership) > smallChangeMaxOwned {
		return fmt.Errorf("%w: the small-change lane owns at most %d paths; requeue the item as Planned delivery", api.ErrConflict, smallChangeMaxOwned)
	}
	return nil
}

const teamQueueCols = `id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,close_json,failure,escalation_seq,released_at,repository,ownership_json,handler_id,handler_run_id,handler_lease_generation,base_commit,acceptance_json,integration_json,serial,owner_integration_json,attempt,retry_of,updated_at`

func scanTeamQueue(row interface{ Scan(...any) error }) (api.TeamQueueEntry, error) {
	var e api.TeamQueueEntry
	var launch, close []byte
	var ownership, acceptance, integration, ownerIntegration string
	err := row.Scan(&e.ID, &e.TaskID, &e.ItemID, &e.ItemRevision, &e.OrderMessageSeq, &e.Template, &e.Position, &e.State, &e.Revision, &e.Host, &e.Cwd, &e.PauseGeneration, &launch, &close, &e.Failure, &e.EscalationSeq, &e.ReleasedAt, &e.Repository, &ownership, &e.HandlerID, &e.HandlerRunID, &e.HandlerLeaseGeneration, &e.BaseCommit, &acceptance, &integration, &e.Serial, &ownerIntegration, &e.Attempt, &e.RetryOf, &e.UpdatedAt)
	if err != nil {
		return e, err
	}
	if ownerIntegration != "" {
		e.OwnerIntegration = new(api.TeamQueueOwnerIntegration)
		if err := json.Unmarshal([]byte(ownerIntegration), e.OwnerIntegration); err != nil {
			return e, err
		}
	}
	if len(launch) > 0 {
		e.LaunchJSON = append([]byte(nil), launch...)
	}
	if len(close) > 0 {
		e.CloseJSON = append([]byte(nil), close...)
	}
	if err := json.Unmarshal([]byte(ownership), &e.Ownership); err != nil {
		return e, err
	}
	if e.Ownership == nil {
		e.Ownership = []string{}
	}
	if acceptance != "" {
		e.Acceptance = new(api.TeamIntegrationAcceptance)
		if err := json.Unmarshal([]byte(acceptance), e.Acceptance); err != nil {
			return e, err
		}
	}
	if integration != "" {
		e.Integration = new(api.TeamIntegrationReady)
		if err := json.Unmarshal([]byte(integration), e.Integration); err != nil {
			return e, err
		}
	}
	return e, nil
}

// ListTeamQueue returns every entry of the project in full. The hub uses it
// internally; the HTTP listing is TeamQueuePage, which trims history.
func (s *Store) ListTeamQueue(ctx context.Context, task string) (api.TeamQueueList, error) {
	if !api.ValidID(task, "tsk") {
		return api.TeamQueueList{}, api.ErrInvalid
	}
	out := api.TeamQueueList{Entries: []api.TeamQueueEntry{}}
	var err error
	if out.ConcurrencyLimit, err = queueConcurrencyLimit(ctx, s.db, task); err != nil {
		return out, err
	}
	if out.Entries, err = s.teamQueueRows(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? ORDER BY position`, task); err != nil {
		return api.TeamQueueList{}, err
	}
	return out, s.explainTeamQueue(ctx, task, &out)
}

// teamQueueRows scans the entries a query selects.
func (s *Store) teamQueueRows(ctx context.Context, query string, args ...any) ([]api.TeamQueueEntry, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.TeamQueueEntry{}
	for rows.Next() {
		e, err := scanTeamQueue(rows)
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, rows.Close()
}

// teamQueueRelease attaches the entry's release job, if any.
func (s *Store) teamQueueRelease(ctx context.Context, e *api.TeamQueueEntry) error {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT record_json FROM release_jobs WHERE task_id=? AND entry_id=? ORDER BY rowid DESC LIMIT 1`, e.TaskID, e.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var j api.ReleaseJob
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return err
	}
	e.Release = &j
	return nil
}

// enrichTeamQueueEntry attaches what a listing shows beside the row: its
// release, reviews, verification, member activity and tokens, handler arm.
func (s *Store) enrichTeamQueueEntry(ctx context.Context, e *api.TeamQueueEntry) error {
	if err := s.teamQueueRelease(ctx, e); err != nil {
		return err
	}
	summary, err := reviewState(ctx, s.db, e.TaskID, e.ItemID)
	if err != nil {
		return err
	}
	e.Reviews = &summary
	if err := s.loadTeamVerification(ctx, e); err != nil {
		return err
	}
	if err := s.loadTeamActivities(ctx, e); err != nil {
		return err
	}
	return attachHandlerArm(ctx, s.db, e)
}

// explainTeamQueue enriches loaded entries and computes block reasons,
// shared-checkout hints, arm waits and stalls. Those read only queued entries
// and entries that hold resources, so history entries may be left out.
func (s *Store) explainTeamQueue(ctx context.Context, task string, out *api.TeamQueueList) error {
	var err error
	activeCount := 0
	for i, entry := range out.Entries {
		if err := s.enrichTeamQueueEntry(ctx, &out.Entries[i]); err != nil {
			return err
		}
		if queueEntryHoldsResources(entry) {
			activeCount++
		}
		if entry.State == "running" && entry.Repository != "" && entry.Acceptance == nil {
			var status string
			if err := s.db.QueryRowContext(ctx, `SELECT status FROM work_items WHERE task_id=? AND id=?`, task, entry.ItemID).Scan(&status); err != nil {
				return err
			}
			if status == "done" {
				out.Entries[i].BlockReason = "Waiting for handler acceptance"
			}
		}
	}
	parallel := queueParallel(out.ConcurrencyLimit)
	var capacityTx *sql.Tx
	var active []api.TeamQueueEntry
	var freeHandler api.Agent
	if parallel {
		capacityTx, err = s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer capacityTx.Rollback()
		for _, entry := range out.Entries {
			if queueEntryHoldsResources(entry) {
				active = append(active, entry)
			}
		}
		if freeHandler, err = freeQueueHandler(ctx, capacityTx, task, active); err != nil {
			return err
		}
	}
	// sharedOnly lists, per queued entry, the active entries that block it
	// only by sharing its checkout, when nothing blocks it by ownership.
	sharedOnly := map[string][]string{}
	for i := range out.Entries {
		if parallel && out.Entries[i].State == "failed" && out.Entries[i].ReleasedAt == "" {
			live, liveErr := liveItemRuns(ctx, capacityTx, task, out.Entries[i].ItemID)
			if liveErr != nil {
				return liveErr
			}
			if live > 0 {
				out.Entries[i].BlockReason = fmt.Sprintf("Failed; %d item-bound runs are still live or uncleaned", live)
			} else {
				out.Entries[i].BlockReason = "Failed; the runner releases it on its next pass"
			}
		}
		if out.Entries[i].State != "queued" {
			continue
		}
		if queueSlotsFull(out.ConcurrencyLimit, activeCount) {
			out.Entries[i].BlockReason = "All team slots are reserved"
		} else if parallel {
			if capacityErr := checkTeamHostAdmission(ctx, capacityTx, out.Entries[i].Host, s.now()); capacityErr != nil {
				out.Entries[i].BlockReason = capacityErr.Error()
			} else if freeHandler.ID == "" {
				out.Entries[i].BlockReason = "No free database handler"
			} else if len(out.Entries[i].Ownership) == 0 && len(active) > 0 {
				if out.Entries[i].Serial {
					out.Entries[i].BlockReason = "Serial: runs alone once the active teams finish"
				} else {
					out.Entries[i].BlockReason = "Unscoped: declare ownership to run beside other teams"
				}
			}
		}
		var sharedWith []string
		overlaps := false
		for j := range out.Entries {
			if i == j {
				continue
			}
			other := out.Entries[j]
			if !queueEntryHoldsResources(other) {
				continue
			}
			sameCwd := out.Entries[i].Cwd != "" && out.Entries[i].Cwd == other.Cwd
			if queueEntryConflicts(out.Entries[i], other) {
				out.Entries[i].BlockedBy = append(out.Entries[i].BlockedBy, other.ID)
				overlaps = true
			} else if sameCwd {
				out.Entries[i].BlockedBy = append(out.Entries[i].BlockedBy, other.ID)
				sharedWith = append(sharedWith, other.ID)
			}
		}
		if len(sharedWith) > 0 && !overlaps {
			// Two teams never edit one checkout; a scoped entry that
			// conflicts only by cwd can move to its own worktree.
			sharedOnly[out.Entries[i].ID] = sharedWith
			if out.Entries[i].BlockReason == "" {
				out.Entries[i].BlockReason = sharedCheckoutHint(out.Entries[i], sharedWith[0])
			}
		}
	}
	var reader queryRower = s.db
	if capacityTx != nil {
		reader = capacityTx
	}
	var holding []api.TeamQueueEntry
	for _, entry := range out.Entries {
		if queueEntryHoldsResources(entry) {
			holding = append(holding, entry)
		}
	}
	if err := explainArmWaits(ctx, reader, out, holding, s.now()); err != nil {
		return err
	}
	if err := s.explainQueueStalls(ctx, reader, capacityTx, out); err != nil {
		return err
	}
	// A stall behind an entry that only shares the checkout keeps the move,
	// which frees the queued entry at once; the stall notice carries it too.
	for i := range out.Entries {
		e := &out.Entries[i]
		if e.Stall == nil {
			continue
		}
		for _, id := range sharedOnly[e.ID] {
			if id == e.Stall.BlockerEntryID {
				e.BlockReason += ". Or: " + sharedCheckoutHint(*e, id)
				break
			}
		}
	}
	return nil
}

func sharedCheckoutHint(e api.TeamQueueEntry, active string) string {
	return fmt.Sprintf("Shares checkout %s with active entry %s; move it: tt team queue scope --task %s --entry %s --new-worktree", e.Cwd, active, e.TaskID, e.ID)
}

func (s *Store) TeamQueuesByHost(ctx context.Context, host string) (api.TeamQueueList, error) {
	if host == "" {
		return api.TeamQueueList{}, api.ErrInvalid
	}
	// A failed entry the owner recorded as integrated still needs the
	// runner's team close, cleanup and finish.
	rows, err := s.db.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE host=? AND (state IN ('queued','launching','running') OR (state='failed' AND owner_integration_json<>'')) ORDER BY task_id,position LIMIT 200`, host)
	if err != nil {
		return api.TeamQueueList{}, err
	}
	defer rows.Close()
	out := api.TeamQueueList{Entries: []api.TeamQueueEntry{}}
	for rows.Next() {
		e, err := scanTeamQueue(rows)
		if err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for i := range out.Entries {
		summary, summaryErr := reviewState(ctx, s.db, out.Entries[i].TaskID, out.Entries[i].ItemID)
		if summaryErr != nil {
			return out, summaryErr
		}
		out.Entries[i].Reviews = &summary
		if err := s.loadTeamVerification(ctx, &out.Entries[i]); err != nil {
			return out, err
		}
		if err := s.loadTeamActivities(ctx, &out.Entries[i]); err != nil {
			return out, err
		}
		if err := attachHandlerArm(ctx, s.db, &out.Entries[i]); err != nil {
			return out, err
		}
	}
	out.HostPolicy, err = readTeamHostPolicy(ctx, s.db, host)
	if err != nil {
		return out, err
	}
	out.HostUsage, err = readTeamHostUsage(ctx, s.db, host)
	return out, err
}

// queueHistorySQL is the SQL form of queueEntryIsHistory.
const queueHistorySQL = `(state='finished' OR (state='failed' AND released_at<>'' AND owner_integration_json=''))`

// queueEntryIsHistory reports whether an entry is done with the queue: it is
// finished, or failed and released without an owner integration the runner
// must still close. History entries never block, stall or hold resources.
func queueEntryIsHistory(e api.TeamQueueEntry) bool {
	return e.State == "finished" || (e.State == "failed" && e.ReleasedAt != "" && e.OwnerIntegration == nil)
}

// TeamQueuePage is the team queue listing: active entries in full, in
// position order, then one newest-first page of history summaries. View
// active leaves history out; Item returns only that item's entry in full.
func (s *Store) TeamQueuePage(ctx context.Context, task string, opts api.TeamQueueListOptions) (api.TeamQueueList, error) {
	if !api.ValidID(task, "tsk") || (opts.View != "" && opts.View != api.TeamQueueViewActive) || (opts.Item != "" && !api.ValidID(opts.Item, "wi")) || opts.Limit < 0 || opts.Limit > api.MaxLimit || opts.After < 0 {
		return api.TeamQueueList{}, api.ErrInvalid
	}
	out := api.TeamQueueList{Entries: []api.TeamQueueEntry{}}
	var err error
	if out.ConcurrencyLimit, err = queueConcurrencyLimit(ctx, s.db, task); err != nil {
		return out, err
	}
	if opts.Item != "" {
		rows, err := s.teamQueueRows(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND item_id=?`, task, opts.Item)
		if err != nil || len(rows) == 0 {
			return out, err
		}
		if queueEntryIsHistory(rows[0]) {
			out.Entries = rows[:1]
			return out, s.enrichTeamQueueEntry(ctx, &out.Entries[0])
		}
	}
	if out.Entries, err = s.teamQueueRows(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND NOT `+queueHistorySQL+` ORDER BY position`, task); err != nil {
		return api.TeamQueueList{}, err
	}
	if err := s.explainTeamQueue(ctx, task, &out); err != nil {
		return out, err
	}
	if opts.Item != "" {
		// The whole active queue explains the entry's blockers; return only it.
		kept := []api.TeamQueueEntry{}
		for _, e := range out.Entries {
			if e.ItemID == opts.Item {
				kept = append(kept, e)
			}
		}
		out.Entries = kept
		return out, nil
	}
	if opts.View == api.TeamQueueViewActive {
		return out, nil
	}
	limit := opts.Limit
	if limit == 0 {
		limit = api.DefaultTeamQueueHistoryLimit
	}
	page := &api.TeamQueueHistoryPage{Limit: limit}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND `+queueHistorySQL, task).Scan(&page.Total); err != nil {
		return out, err
	}
	// Positions of non-queued entries never change, so a position cursor is
	// stable while entries finish between pages.
	history, err := s.teamQueueRows(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND `+queueHistorySQL+` AND (?=0 OR position<?) ORDER BY position DESC LIMIT ?`, task, opts.After, opts.After, limit+1)
	if err != nil {
		return out, err
	}
	if len(history) > limit {
		history = history[:limit]
		page.NextAfter = history[limit-1].Position
	}
	for i := range history {
		if err := s.enrichTeamQueueEntry(ctx, &history[i]); err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, summarizeTeamQueueEntry(history[i]))
	}
	out.History = page
	return out, nil
}

// Summary bounds for a history entry in a listing.
const (
	summaryTitleBytes    = 200
	summaryEvidenceBytes = 500
	summaryChangedFiles  = 20
)

// teamShape reads the team shape from a frozen launch: a plan review team
// when a member has the Plan review role, else a plan-only team.
func teamShape(launch json.RawMessage) string {
	if len(launch) == 0 {
		return ""
	}
	var l struct {
		Members []struct {
			Fields struct {
				Role string `json:"role"`
			} `json:"fields"`
		} `json:"members"`
	}
	if json.Unmarshal(launch, &l) != nil || len(l.Members) == 0 {
		return ""
	}
	for _, m := range l.Members {
		if m.Fields.Role == "Plan review" {
			return "plan-review"
		}
	}
	return "plan-only"
}

// summarizeTeamQueueEntry trims a history entry for a listing: no launch,
// close or activities, and reviews, verification, release and evidence cut
// to what the Delivery view and tt show. Tokens and identity stay.
func summarizeTeamQueueEntry(e api.TeamQueueEntry) api.TeamQueueEntry {
	e.Summary = true
	e.TeamShape = teamShape(e.LaunchJSON)
	e.LaunchJSON, e.CloseJSON, e.Activities = nil, nil, nil
	if r := e.Reviews; r != nil {
		sum := api.ReviewConvergence{ItemID: r.ItemID, History: r.History, Disposition: r.Disposition, Scopes: []api.ReviewScope{}, Rounds: make([]api.ReviewRound, 0, len(r.Rounds)), FollowUps: make([]api.ReviewFollowUp, 0, len(r.FollowUps)), Focused: []api.FocusedReview{}}
		for _, round := range r.Rounds {
			round.Findings, round.Criteria, round.VerificationCriteria, round.Blockers = nil, nil, nil, nil
			sum.Rounds = append(sum.Rounds, round)
		}
		for _, f := range r.FollowUps {
			sum.FollowUps = append(sum.FollowUps, api.ReviewFollowUp{ItemID: f.ItemID, MessageSeq: f.MessageSeq, Finding: api.ReviewFinding{ID: f.Finding.ID, Title: clip(f.Finding.Title, summaryTitleBytes)}})
		}
		e.Reviews = &sum
	}
	if v := e.Verification; v != nil {
		sum := api.VerificationSummary{State: v.State, Commit: v.Commit}
		for _, c := range v.Checks {
			if c.Status != "pass" || c.KnownFailure || c.NowPassing {
				sum.Checks = append(sum.Checks, c)
			}
		}
		e.Verification = &sum
	}
	if r := e.Release; r != nil {
		sum := *r
		sum.Plan = api.VerificationPlan{}
		sum.IntegratedPlan, sum.IntegratedVerification, sum.Reconciliations = nil, nil, nil
		e.Release = &sum
	}
	if a := e.Acceptance; a != nil {
		sum := *a
		sum.Evidence = clip(sum.Evidence, summaryEvidenceBytes)
		sum.ResolvedKnownFailures = nil
		e.Acceptance = &sum
	}
	if i := e.Integration; i != nil {
		sum := *i
		sum.Evidence = clip(sum.Evidence, summaryEvidenceBytes)
		e.Integration = &sum
	}
	if o := e.OwnerIntegration; o != nil {
		sum := *o
		sum.Evidence = clip(sum.Evidence, summaryEvidenceBytes)
		if len(sum.ChangedFiles) > summaryChangedFiles {
			sum.ChangedFiles = sum.ChangedFiles[:summaryChangedFiles]
		}
		e.OwnerIntegration = &sum
	}
	return e
}

func (s *Store) GetTeamQueueEntry(ctx context.Context, task, id string) (api.TeamQueueEntry, error) {
	if !api.ValidID(task, "tsk") || !validTeamQueueID(id) {
		return api.TeamQueueEntry{}, api.ErrInvalid
	}
	e, err := scanTeamQueue(s.db.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND id=?`, task, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, api.ErrNotFound
	}
	if err == nil {
		summary, summaryErr := reviewState(ctx, s.db, task, e.ItemID)
		if summaryErr != nil {
			return e, summaryErr
		}
		if summary.History == "recorded" {
			e.Reviews = &summary
		}
	}
	if err == nil {
		err = s.loadTeamVerification(ctx, &e)
	}
	if err == nil {
		err = attachHandlerArm(ctx, s.db, &e)
	}
	// One entry carries at least what any listing shows for it.
	if err == nil {
		err = s.teamQueueRelease(ctx, &e)
	}
	if err == nil {
		err = s.loadTeamActivities(ctx, &e)
	}
	return e, err
}

// TeamQueueAction serializes every state change with ordinary project writes and
// keeps retry results durable. Effect attempts are recorded before host effects.
func (s *Store) TeamQueueAction(ctx context.Context, task string, req api.TeamQueueRequest) (api.TeamQueueEntry, error) {
	var zero api.TeamQueueEntry
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) {
		return zero, api.ErrInvalid
	}
	identity := req
	if identity.Operation == "release" || identity.Operation == "accept" || identity.Operation == "owner_integrated" {
		// A lost response is replayable after the release increments revision.
		identity.ExpectedRevision = 0
	}
	if identity.Operation == "stall_notice" {
		// One notice per stall: every queued entry behind the same blocker
		// replays the first one.
		identity = api.TeamQueueRequest{RequestID: req.RequestID, Operation: req.Operation}
	}
	b, _ := json.Marshal(identity)
	h := sha256.Sum256(b)
	hash := hex.EncodeToString(h[:])
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var oldHash string
	var old []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload_hash,result_json FROM team_queue_requests WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&oldHash, &old)
	if err == nil {
		if oldHash != hash {
			return zero, fmt.Errorf("%w: team queue retry differs", api.ErrConflict)
		}
		if req.Operation == "manual" || req.Operation == "claim" {
			var reservedToken string
			if lookupErr := s.db.QueryRowContext(ctx, `SELECT token FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, req.EntryID).Scan(&reservedToken); lookupErr != nil || reservedToken != req.RequestID {
				return zero, fmt.Errorf("%w: launch reservation has since been released", api.ErrConflict)
			}
		}
		var e api.TeamQueueEntry
		err = json.Unmarshal(old, &e)
		return e, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	t, err := s.GetTask(ctx, task)
	if err != nil {
		return zero, err
	}
	if t.Status != api.TaskOpen {
		return zero, api.ErrClosed
	}
	if req.Operation == "manual" {
		agents, err := s.ListAgents(ctx, task)
		if err != nil {
			return zero, err
		}
		handlers := 0
		for _, a := range agents {
			if a.Role == api.AgentRoleDatabaseHandler && a.Online && a.Status != api.AgentClosed && a.Status != api.AgentExited && a.Status != api.AgentRetired {
				handlers++
			}
		}
		if handlers != 1 {
			return zero, fmt.Errorf("%w: exactly one available database handler is required", api.ErrConflict)
		}
	}
	var stalled api.TeamQueueEntry
	var stallEnvelope api.Envelope
	if req.Operation == "stall_notice" && t.PauseState != api.ProjectPauseActive {
		return zero, fmt.Errorf("%w: the project is paused; its queue has no stalls", api.ErrConflict)
	}
	if req.Operation == "stall_notice" {
		// The list reads through the single connection, so the stall is
		// recomputed before the write transaction opens.
		if stalled, stallEnvelope, err = s.stallNotice(ctx, task, req); err != nil {
			return zero, err
		}
	}
	if req.Operation == "claim" {
		// Expired provider-limit episodes close before the draw, in their
		// own transaction, so a claim that waits does not undo the clearing.
		if err := s.sweepArmLimits(ctx, task); err != nil {
			return zero, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	now := ts(s.now())
	var e api.TeamQueueEntry
	switch req.Operation {
	case "stall_notice":
		// A notice from the queue runner to the Board: no recipient, no
		// escalation ref and no obligation.
		if _, err := s.insertMessage(ctx, tx, t, api.PostMessageRequest{Envelope: &stallEnvelope}, api.Agent{}, api.Caller{Node: "team_queue", User: "runner"}, false, false); err != nil {
			return zero, err
		}
		e = stalled
	case "set_host_policy":
		if req.Host == "" || !validLimiterDomain(req.LimiterDomain) || len(req.LimiterDomain) > 255 || req.HostPolicyVersion < 1 || req.HostMaxSessions < 1 || req.HostMaxPolling < 1 || req.HostMaxRelayBindings < 1 || req.HostMaxRequestsPerMinute < 1 || req.HostMaxBurst < 1 || req.HostHeadroomPercent < 1 || req.HostHeadroomPercent >= 100 || req.HostMaxRequestsPerMinute > 1000000 || req.HostMaxBurst > 100000 || req.HostMinFreeDiskMiB < 0 {
			return zero, api.ErrInvalid
		}
		expires, parseErr := time.Parse(time.RFC3339, req.HostPolicyExpires)
		if parseErr != nil || !expires.After(s.now()) {
			return zero, api.ErrInvalid
		}
		var previous int64
		lookupErr := tx.QueryRowContext(ctx, `SELECT version FROM team_host_policies WHERE host=?`, req.Host).Scan(&previous)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return zero, lookupErr
		}
		if previous >= req.HostPolicyVersion {
			return zero, fmt.Errorf("%w: host policy version must increase", api.ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_host_policies(host,version,expires_at,max_sessions,max_polling,limiter_domain,max_relay_bindings,max_requests_per_minute,max_burst,headroom_percent,min_free_disk_mib) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(host) DO UPDATE SET version=excluded.version,expires_at=excluded.expires_at,max_sessions=excluded.max_sessions,max_polling=excluded.max_polling,limiter_domain=excluded.limiter_domain,max_relay_bindings=excluded.max_relay_bindings,max_requests_per_minute=excluded.max_requests_per_minute,max_burst=excluded.max_burst,headroom_percent=excluded.headroom_percent,min_free_disk_mib=excluded.min_free_disk_mib`, req.Host, req.HostPolicyVersion, req.HostPolicyExpires, req.HostMaxSessions, req.HostMaxPolling, req.LimiterDomain, req.HostMaxRelayBindings, req.HostMaxRequestsPerMinute, req.HostMaxBurst, req.HostHeadroomPercent, req.HostMinFreeDiskMiB); err != nil {
			return zero, err
		}
		e = api.TeamQueueEntry{TaskID: task, Host: req.Host, State: "host_policy", Revision: req.HostPolicyVersion}
	case "observe_host":
		u := req.HostUsage
		if req.Host == "" || u == nil || u.Host != req.Host || !validLimiterDomain(u.LimiterDomain) || u.PolicyVersion < 1 || u.RelayBindings < 0 || !validContextDigest(u.SourceDigest) || (u.FreeDiskMiB != nil && *u.FreeDiskMiB < 0) {
			return zero, api.ErrInvalid
		}
		observed, parseErr := time.Parse(time.RFC3339Nano, u.ObservedAt)
		if parseErr != nil || observed.After(s.now().Add(5*time.Second)) || s.now().Sub(observed) > 30*time.Second {
			return zero, fmt.Errorf("%w: host usage observation is stale", api.ErrConflict)
		}
		policy, err := readTeamHostPolicy(ctx, tx, req.Host)
		if err != nil {
			return zero, err
		}
		if policy == nil || policy.LimiterDomain != u.LimiterDomain || policy.Version != u.PolicyVersion {
			return zero, fmt.Errorf("%w: host usage limiter domain differs from policy", api.ErrConflict)
		}
		previous, err := readTeamHostUsage(ctx, tx, req.Host)
		if err != nil {
			return zero, err
		}
		if previous != nil && previous.PolicyVersion == u.PolicyVersion {
			previousAt, parseErr := time.Parse(time.RFC3339Nano, previous.ObservedAt)
			if parseErr != nil || !observed.After(previousAt) && (observed.Before(previousAt) || previous.SourceDigest != u.SourceDigest || previous.Complete != u.Complete || previous.RelayBindings != u.RelayBindings || !sameFreeDisk(previous.FreeDiskMiB, u.FreeDiskMiB)) {
				return zero, fmt.Errorf("%w: host usage observation regressed", api.ErrConflict)
			}
		}
		var freeDisk sql.NullInt64
		if u.FreeDiskMiB != nil {
			freeDisk = sql.NullInt64{Int64: *u.FreeDiskMiB, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_host_usage(host,limiter_domain,policy_version,observed_at,relay_bindings,complete,source_digest,free_disk_mib) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(host) DO UPDATE SET limiter_domain=excluded.limiter_domain,policy_version=excluded.policy_version,observed_at=excluded.observed_at,relay_bindings=excluded.relay_bindings,complete=excluded.complete,source_digest=excluded.source_digest,free_disk_mib=excluded.free_disk_mib`, u.Host, u.LimiterDomain, u.PolicyVersion, u.ObservedAt, u.RelayBindings, u.Complete, u.SourceDigest, freeDisk); err != nil {
			return zero, err
		}
		e = api.TeamQueueEntry{TaskID: task, Host: req.Host, State: "host_usage"}
	case "set_limit":
		if req.ConcurrencyLimit < 0 {
			return zero, api.ErrInvalid
		}
		if queueParallel(req.ConcurrencyLimit) {
			if req.Host == "" {
				return zero, api.ErrInvalid
			}
			if err := checkTeamHostAdmission(ctx, tx, req.Host, s.now()); err != nil {
				return zero, err
			}
			var unverified int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND (state='queued' OR `+queueHoldsSQL+`) AND (repository='' OR base_commit='')`, task).Scan(&unverified); err != nil {
				return zero, err
			}
			if unverified != 0 {
				return zero, fmt.Errorf("%w: parallel queues need frozen repository and base for every outstanding item", api.ErrConflict)
			}
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND `+queueHoldsSQL, task).Scan(&active); err != nil {
			return zero, err
		}
		if req.ConcurrencyLimit > 0 && active > req.ConcurrencyLimit {
			return zero, fmt.Errorf("%w: active safety reservations exceed requested limit", api.ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_queue_settings(task_id,concurrency_limit) VALUES(?,?) ON CONFLICT(task_id) DO UPDATE SET concurrency_limit=excluded.concurrency_limit`, task, req.ConcurrencyLimit); err != nil {
			return zero, err
		}
		e = api.TeamQueueEntry{TaskID: task, State: "settings", Revision: int64(req.ConcurrencyLimit)}
	case "manual_release":
		if !api.ValidID(req.ItemID, "wi") || req.OrderMessageSeq < 1 || req.ReservationToken != fmt.Sprintf("manual-%s-%s-%d", task, req.ItemID, req.OrderMessageSeq) || t.Orchestrator != "" || t.CleanupPending != 0 {
			return zero, api.ErrConflict
		}
		var reservedItem, reservedEntry, token, state string
		err := tx.QueryRowContext(ctx, `SELECT item_id,entry_id,token,state FROM team_launch_reservations WHERE task_id=? AND entry_id=''`, task).Scan(&reservedItem, &reservedEntry, &token, &state)
		if err != nil || reservedItem != req.ItemID || reservedEntry != "" || token != req.ReservationToken || (state != "reserved" && state != "launching") {
			return zero, fmt.Errorf("%w: exact manual reservation is missing", api.ErrConflict)
		}
		if state == "launching" {
			var proof struct {
				Task    string `json:"task"`
				Item    string `json:"item"`
				Order   int64  `json:"order"`
				Members []struct {
					AgentID string `json:"agentId"`
					State   string `json:"state"`
					RunID   string `json:"runId"`
				} `json:"members"`
			}
			if !req.SessionsChecked || json.Unmarshal(req.ManualJournal, &proof) != nil || proof.Task != task || proof.Item != req.ItemID || proof.Order != req.OrderMessageSeq || len(proof.Members) == 0 {
				return zero, fmt.Errorf("%w: exact manual journal and host session proof are required", api.ErrConflict)
			}
			seen := map[string]bool{}
			for _, m := range proof.Members {
				if !api.ValidID(m.AgentID, "agt") || seen[m.AgentID] || (m.State != "unstarted" && m.State != "uncertain" && m.State != "started") {
					return zero, api.ErrInvalid
				}
				seen[m.AgentID] = true
				if m.State == "unstarted" {
					continue
				}
				var run, status string
				var clean bool
				err := tx.QueryRowContext(ctx, `SELECT run_id,status,cleanup_done FROM agents WHERE task_id=? AND id=?`, task, m.AgentID).Scan(&run, &status, &clean)
				if errors.Is(err, sql.ErrNoRows) && m.State == "uncertain" && m.RunID == "" {
					continue
				}
				if err != nil || m.RunID != run || status != api.AgentClosed || !clean {
					return zero, fmt.Errorf("%w: manual attempted run lacks exact close and cleanup", api.ErrConflict)
				}
			}
		}
		if err := queueReleaseSafe(ctx, tx, task, req.ItemID, "", "", nil, nil); err != nil {
			return zero, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM team_launch_reservations WHERE task_id=? AND item_id=? AND token=? AND entry_id=''`, task, req.ItemID, token); err != nil {
			return zero, err
		}
		e = api.TeamQueueEntry{TaskID: task, ItemID: req.ItemID, OrderMessageSeq: req.OrderMessageSeq, State: "released", ReleasedAt: now}
	case "manual":
		if !api.ValidID(req.ItemID, "wi") || req.OrderMessageSeq < 1 || req.PauseGeneration != t.PauseGeneration || t.Orchestrator != "" || t.CleanupPending != 0 || t.PauseState != api.ProjectPauseActive {
			return zero, api.ErrConflict
		}
		if req.Host != "" {
			policy, err := readTeamHostPolicy(ctx, tx, req.Host)
			if err != nil {
				return zero, err
			}
			if policy != nil {
				if err := checkTeamHostCapacity(ctx, tx, req.Host, s.now(), 1); err != nil {
					return zero, err
				}
			}
		}
		var activeReservations int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_launch_reservations WHERE task_id=?`, task).Scan(&activeReservations); err != nil {
			return zero, err
		}
		if activeReservations != 0 {
			return zero, fmt.Errorf("%w: another team launch is reserved", api.ErrConflict)
		}
		var queued int
		_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND item_id=?`, task, req.ItemID).Scan(&queued)
		if queued > 0 {
			return zero, fmt.Errorf("%w: item is already in team queue", api.ErrConflict)
		}
		var blocked int
		_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND state='failed' AND released_at=''`, task).Scan(&blocked)
		if blocked > 0 {
			return zero, fmt.Errorf("%w: unresolved failed queue entry", api.ErrConflict)
		}
		item, err := getWorkItem(tx, ctx, task, req.ItemID)
		if err != nil {
			return zero, err
		}
		if item.Status == "done" || item.Status == "dismissed" {
			return zero, api.ErrConflict
		}
		if err := recordedTeamOrder(ctx, tx, task, req.ItemID, item.Revision, req.OrderMessageSeq); err != nil {
			return zero, err
		}
		if err := requireConfirmedTeamOrder(ctx, tx, task, req.ItemID, item.Revision, req.OrderMessageSeq); err != nil {
			return zero, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO team_launch_reservations(task_id,entry_id,item_id,token,pause_generation,state,created_at,host) VALUES(?,?,?,?,?,?,?,?)`, task, "", req.ItemID, req.RequestID, t.PauseGeneration, "reserved", now, req.Host)
		if err != nil {
			return zero, fmt.Errorf("%w: another team launch is reserved", api.ErrConflict)
		}
		e = api.TeamQueueEntry{TaskID: task, ItemID: req.ItemID, OrderMessageSeq: req.OrderMessageSeq, State: "launching", PauseGeneration: t.PauseGeneration}
	case "add":
		if !api.ValidID(req.ItemID, "wi") || req.OrderMessageSeq < 1 || req.Host == "" || req.Cwd == "" {
			return zero, api.ErrInvalid
		}
		template := req.Template
		if template == "" {
			template = "planned"
		}
		if template != "planned" && template != "small" {
			return zero, fmt.Errorf("%w: unknown queue template %q; use planned or small", api.ErrInvalid, req.Template)
		}
		ownership, err := canonicalQueueOwnership(req.Ownership)
		if err != nil {
			return zero, err
		}
		if template == "small" {
			if err := smallChangeOwnership(ownership, req.Serial); err != nil {
				return zero, err
			}
		}
		if strings.Contains(req.Repository, "\x00") || len(req.Repository) > 1024 {
			return zero, api.ErrInvalid
		}
		if req.Repository != "" && !validGitCommit(req.BaseCommit) {
			return zero, api.ErrInvalid
		}
		limit, err := queueConcurrencyLimit(ctx, tx, task)
		if err != nil {
			return zero, err
		}
		if queueParallel(limit) && (req.Repository == "" || req.BaseCommit == "") {
			return zero, fmt.Errorf("%w: parallel queue entry needs a frozen repository and base", api.ErrConflict)
		}
		// An entry either declares what it will change or runs alone. A
		// serial project's unscoped entries are serial by definition.
		if req.Serial && len(ownership) != 0 {
			return zero, fmt.Errorf("%w: a serial entry declares no ownership", api.ErrInvalid)
		}
		serial := req.Serial || (!queueParallel(limit) && len(ownership) == 0)
		if len(ownership) == 0 && !serial {
			return zero, fmt.Errorf("%w: a parallel queue entry needs ownership: pass --owns, have the handler record --owns at scope confirmation, or mark it --serial", api.ErrConflict)
		}
		item, err := getWorkItem(tx, ctx, task, req.ItemID)
		if err != nil {
			return zero, err
		}
		if item.Status == "done" || item.Status == "dismissed" {
			return zero, fmt.Errorf("%w: item is terminal", api.ErrConflict)
		}
		if template == "small" && item.Kind != "bug" {
			return zero, fmt.Errorf("%w: the small-change lane admits only bugs; queue a %s as Planned delivery", api.ErrConflict, item.Kind)
		}
		var liveTeam int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.status NOT IN ('closed','exited')`, task, req.ItemID).Scan(&liveTeam); err != nil {
			return zero, err
		}
		if liveTeam > 0 {
			return zero, fmt.Errorf("%w: item already has a live team", api.ErrConflict)
		}
		if err := recordedTeamOrder(ctx, tx, task, req.ItemID, item.Revision, req.OrderMessageSeq); err != nil {
			return zero, err
		}
		if err := requireConfirmedTeamOrder(ctx, tx, task, req.ItemID, item.Revision, req.OrderMessageSeq); err != nil {
			return zero, err
		}
		var maxPos int64
		_ = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),0) FROM team_queue_entries WHERE task_id=?`, task).Scan(&maxPos)
		e = api.TeamQueueEntry{ID: api.NewID("tqe"), TaskID: task, ItemID: req.ItemID, ItemRevision: item.Revision, OrderMessageSeq: req.OrderMessageSeq, Template: template, Position: maxPos + 1, State: "queued", Revision: 1, Host: req.Host, Cwd: req.Cwd, Repository: req.Repository, Ownership: ownership, BaseCommit: req.BaseCommit, Serial: serial, Attempt: 1, UpdatedAt: now}
		ownedJSON, _ := json.Marshal(ownership)
		_, err = tx.ExecContext(ctx, `INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,repository,ownership_json,base_commit,serial,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, task, e.ItemID, e.ItemRevision, e.OrderMessageSeq, e.Template, e.Position, e.State, e.Revision, e.Host, e.Cwd, e.Repository, string(ownedJSON), e.BaseCommit, serial, now, now)
		if err != nil {
			return zero, fmt.Errorf("%w: duplicate item or queue entry: %v", api.ErrConflict, err)
		}
	case "remove", "reorder", "claim", "freeze", "attempt", "started", "running", "replace_lead", "close", "close_refresh", "accept", "finish", "fail", "release", "scope", "owner_integrated":
		if !validTeamQueueID(req.EntryID) {
			return zero, api.ErrInvalid
		}
		e, err = scanTeamQueue(tx.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND id=?`, task, req.EntryID))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return zero, api.ErrNotFound
			}
			return zero, err
		}
		if e.Revision != req.ExpectedRevision {
			return zero, fmt.Errorf("%w: entry revision changed", api.ErrConflict)
		}
		switch req.Operation {
		case "scope":
			ownership, err := canonicalQueueOwnership(req.Ownership)
			if err != nil {
				return zero, err
			}
			if len(ownership) == 0 {
				return zero, fmt.Errorf("%w: scope needs at least one owned path", api.ErrInvalid)
			}
			// A small entry never widens past its cap; splitting an owned
			// directory into the files it changed is still a narrowing.
			if e.Template == "small" && len(ownership) > smallChangeMaxOwned && !queueNarrows(ownership, e.Ownership) {
				return zero, fmt.Errorf("%w: the small-change lane owns at most %d paths; requeue the item as Planned delivery", api.ErrConflict, smallChangeMaxOwned)
			}
			if req.Cwd != "" {
				// A queued entry may move off a shared checkout into its own
				// worktree of the same repository; its base stays frozen.
				if e.State != "queued" {
					return zero, fmt.Errorf("%w: only a queued entry can move to its own worktree", api.ErrConflict)
				}
				if !filepath.IsAbs(req.Cwd) || filepath.Clean(req.Cwd) != req.Cwd || strings.Contains(req.Cwd, "\x00") || len(req.Cwd) > 4096 {
					return zero, fmt.Errorf("%w: the new worktree must be a clean absolute path", api.ErrInvalid)
				}
				if e.Repository == "" || req.Repository != e.Repository {
					return zero, fmt.Errorf("%w: the new worktree must be in the entry's repository", api.ErrConflict)
				}
				var user string
				err := tx.QueryRowContext(ctx, `SELECT id FROM team_queue_entries WHERE task_id=? AND id<>? AND cwd=? AND (state='queued' OR `+queueHoldsSQL+`) ORDER BY position LIMIT 1`, task, e.ID, req.Cwd).Scan(&user)
				if err == nil {
					return zero, fmt.Errorf("%w: entry %s already uses %s", api.ErrConflict, user, req.Cwd)
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return zero, err
				}
			}
			failedHold := e.State == "failed" && e.ReleasedAt == ""
			if e.State != "queued" && e.State != "launching" && e.State != "running" && !failedHold {
				return zero, fmt.Errorf("%w: only a queued, launching, running or unreleased failed entry can be scoped", api.ErrConflict)
			}
			if err := queueScopeAuthority(ctx, tx, task, e, req); err != nil {
				return zero, err
			}
			if failedHold {
				// A failed entry may give way, never take more: the owner
				// narrows it while its team is still being reconciled.
				if req.HandlerAgentID != "" || req.LeadAgentID != "" {
					return zero, fmt.Errorf("%w: only the owner may scope a failed entry", api.ErrConflict)
				}
				if !queueNarrows(ownership, e.Ownership) {
					return zero, fmt.Errorf("%w: a failed entry can only be narrowed; each path must lie under its current ownership", api.ErrConflict)
				}
			}
			if e.State != "queued" {
				// An admitted team may narrow its declaration, but it may not
				// widen into paths another active team holds.
				scoped := e
				scoped.Ownership = ownership
				rows, err := tx.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND id<>? AND `+queueHoldsSQL+` ORDER BY position`, task, e.ID)
				if err != nil {
					return zero, err
				}
				var overlap string
				for rows.Next() {
					other, scanErr := scanTeamQueue(rows)
					if scanErr != nil {
						rows.Close()
						return zero, scanErr
					}
					if overlap == "" && queueEntryConflicts(scoped, other) {
						overlap = other.ID
					}
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					return zero, err
				}
				if overlap != "" {
					return zero, fmt.Errorf("%w: ownership overlaps active entry %s", api.ErrConflict, overlap)
				}
			}
			e.Ownership = ownership
			e.Serial = false
			if req.Cwd != "" {
				e.Cwd = req.Cwd
			}
		case "owner_integrated":
			if req.HandlerAgentID != "" || req.HandlerRunID != "" || req.LeadAgentID != "" || req.LeadRunID != "" {
				return zero, fmt.Errorf("%w: only the owner records an integration", api.ErrConflict)
			}
			if !validGitCommit(req.OwnerIntegrationCommit) || len(req.OwnerIntegrationEvidence) > 2000 || strings.ContainsRune(req.OwnerIntegrationEvidence, '\x00') || len(req.ChangedFiles) > 4096 {
				return zero, api.ErrInvalid
			}
			if e.ReleasedAt != "" || (e.State != "running" && e.State != "failed") {
				return zero, fmt.Errorf("%w: only a running or unreleased failed entry can be marked integrated", api.ErrConflict)
			}
			if e.State == "failed" {
				var plan struct {
					Members []struct {
						State string `json:"state"`
					} `json:"members"`
				}
				if len(e.LaunchJSON) != 0 && json.Unmarshal(e.LaunchJSON, &plan) != nil {
					return zero, api.ErrConflict
				}
				for _, m := range plan.Members {
					if m.State == "uncertain" {
						return zero, fmt.Errorf("%w: the failed launch has an uncertain spawn; release it with tt team queue release", api.ErrConflict)
					}
				}
			}
			// A job still in the deployment path owns the candidate. Released,
			// rolled back, refused and superseded jobs are finished.
			var jobID, jobState string
			jobErr := tx.QueryRowContext(ctx, `SELECT id,state FROM release_jobs WHERE task_id=? AND entry_id=? AND state IN ('verified','claimed','merged','blocked')`, task, e.ID).Scan(&jobID, &jobState)
			if jobErr == nil {
				return zero, fmt.Errorf("%w: release job %s is %s; the deployment path owns this candidate", api.ErrConflict, jobID, jobState)
			}
			if !errors.Is(jobErr, sql.ErrNoRows) {
				return zero, jobErr
			}
			changed := make([]string, 0, len(req.ChangedFiles))
			for _, file := range req.ChangedFiles {
				if file == "" || len(file) > 1024 || strings.ContainsRune(file, '\x00') {
					return zero, api.ErrInvalid
				}
				changed = append(changed, file)
			}
			base := e.BaseCommit
			if e.Acceptance != nil {
				base = e.Acceptance.BaseCommit
			}
			e.OwnerIntegration = &api.TeamQueueOwnerIntegration{Commit: req.OwnerIntegrationCommit, BaseCommit: base, Evidence: req.OwnerIntegrationEvidence, ChangedFiles: changed, At: now}
			// The released entry no longer holds ownership; recording the
			// changed files keeps its list truthful where they are valid.
			if narrowed, err := canonicalQueueOwnership(changed); err == nil && len(narrowed) > 0 {
				e.Ownership = narrowed
				e.Serial = false
			}
			e.ReleasedAt = now
			if _, err = tx.ExecContext(ctx, `DELETE FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, e.ID); err != nil {
				return zero, err
			}
		case "release":
			limit, err := queueConcurrencyLimit(ctx, tx, task)
			if err != nil {
				return zero, err
			}
			if e.State != "failed" || e.ReleasedAt != "" || (!queueParallel(limit) && (t.Orchestrator != "" || t.CleanupPending != 0)) {
				return zero, api.ErrConflict
			}
			if req.ReleaseProof != nil && (!req.SessionsChecked || req.Host != e.Host) {
				return zero, fmt.Errorf("%w: saved launch host proof is required", api.ErrConflict)
			}
			if err := queueReleaseSafe(ctx, tx, task, e.ItemID, e.ID, e.Host, e.LaunchJSON, req.ReleaseProof); err != nil {
				return zero, err
			}
			var reservedEntry string
			err = tx.QueryRowContext(ctx, `SELECT entry_id FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, e.ID).Scan(&reservedEntry)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return zero, err
			}
			if err == nil && reservedEntry != e.ID {
				return zero, fmt.Errorf("%w: another team holds the reservation", api.ErrConflict)
			}
			if err == nil {
				if _, err = tx.ExecContext(ctx, `DELETE FROM team_launch_reservations WHERE task_id=? AND entry_id=? AND item_id=?`, task, e.ID, e.ItemID); err != nil {
					return zero, err
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE item_team_leads SET state='closed',revision=revision+1 WHERE task_id=? AND item_id=? AND state<>'closed'`, task, e.ItemID); err != nil {
				return zero, err
			}
			e.ReleasedAt = now
		case "remove":
			if e.State != "queued" {
				return zero, fmt.Errorf("%w: only queued entries can be removed", api.ErrConflict)
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM team_queue_entries WHERE id=?`, e.ID)
			if err != nil {
				return zero, err
			}
		case "reorder":
			if e.State != "queued" {
				return zero, fmt.Errorf("%w: only queued entries can be reordered", api.ErrConflict)
			}
			if req.BeforeID != "" {
				var existing string
				if err = tx.QueryRowContext(ctx, `SELECT id FROM team_queue_entries WHERE task_id=? AND id=? AND state='queued'`, task, req.BeforeID).Scan(&existing); err != nil {
					return zero, fmt.Errorf("%w: before entry is not queued", api.ErrConflict)
				}
			}
			// Keep integer positions gapless in a transaction; the source row is
			// removed from the ordering before insertion at the requested point.
			rows, err := tx.QueryContext(ctx, `SELECT id FROM team_queue_entries WHERE task_id=? AND state='queued' AND id<>? ORDER BY position`, task, e.ID)
			if err != nil {
				return zero, err
			}
			var ids []string
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					rows.Close()
					return zero, err
				}
				ids = append(ids, id)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return zero, err
			}
			index := len(ids)
			if req.BeforeID != "" {
				index = -1
				for i, id := range ids {
					if id == req.BeforeID {
						index = i
						break
					}
				}
				if index < 0 {
					return zero, api.ErrConflict
				}
			}
			ids = append(ids, "")
			copy(ids[index+1:], ids[index:])
			ids[index] = e.ID
			var base int64
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),0) FROM team_queue_entries WHERE task_id=? AND state<>'queued'`, task).Scan(&base); err != nil {
				return zero, err
			}
			for i, id := range ids {
				if _, err = tx.ExecContext(ctx, `UPDATE team_queue_entries SET position=?,revision=revision+1,updated_at=? WHERE id=?`, base+int64(i)+1, now, id); err != nil {
					return zero, err
				}
			}
		case "claim":
			limit, err := queueConcurrencyLimit(ctx, tx, task)
			if err != nil {
				return zero, err
			}
			if e.State != "queued" || t.PauseState != api.ProjectPauseActive || (!queueParallel(limit) && (t.Orchestrator != "" || t.CleanupPending != 0)) || req.Host != e.Host || req.PauseGeneration != t.PauseGeneration {
				return zero, fmt.Errorf("%w: project is not launchable", api.ErrConflict)
			}
			if err := requireConfirmedTeamOrder(ctx, tx, task, e.ItemID, e.ItemRevision, e.OrderMessageSeq); err != nil {
				return zero, err
			}
			var manual int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_launch_reservations WHERE task_id=? AND entry_id=''`, task).Scan(&manual); err != nil {
				return zero, err
			}
			if manual != 0 {
				return zero, fmt.Errorf("%w: manual launch is reserved", api.ErrConflict)
			}
			rows, err := tx.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND (state='queued' OR `+queueHoldsSQL+`) ORDER BY position`, task)
			if err != nil {
				return zero, err
			}
			var candidates, activeEntries []api.TeamQueueEntry
			for rows.Next() {
				candidate, scanErr := scanTeamQueue(rows)
				if scanErr != nil {
					rows.Close()
					return zero, scanErr
				}
				if candidate.State == "queued" {
					candidates = append(candidates, candidate)
				} else {
					activeEntries = append(activeEntries, candidate)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return zero, err
			}
			if queueSlotsFull(limit, len(activeEntries)) {
				return zero, fmt.Errorf("%w: all team slots are reserved", api.ErrConflict)
			}
			if queueParallel(limit) {
				if err := checkTeamHostAdmission(ctx, tx, e.Host, s.now()); err != nil {
					return zero, err
				}
			}
			var head string
			for _, candidate := range candidates {
				conflict := false
				for _, active := range activeEntries {
					if candidate.Cwd == active.Cwd || queueEntryConflicts(candidate, active) {
						conflict = true
						break
					}
				}
				if !conflict {
					head = candidate.ID
					break
				}
			}
			if head != e.ID {
				return zero, fmt.Errorf("%w: not queue head", api.ErrConflict)
			}
			chosen, armLease, err := leaseQueueHandler(ctx, tx, task, e.ID, activeEntries, s.now())
			if err != nil {
				return zero, err
			}
			if chosen.ID == "" {
				return zero, fmt.Errorf("%w: no available database handler lease", api.ErrConflict)
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO team_launch_reservations(task_id,entry_id,item_id,token,pause_generation,state,created_at) VALUES(?,?,?,?,?,?,?)`, task, e.ID, e.ItemID, req.RequestID, t.PauseGeneration, "reserved", now); err != nil {
				return zero, err
			}
			e.State = "launching"
			e.PauseGeneration = t.PauseGeneration
			e.HandlerID, e.HandlerRunID = chosen.ID, chosen.RunID
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(handler_lease_generation),0)+1 FROM team_queue_entries WHERE handler_id=?`, chosen.ID).Scan(&e.HandlerLeaseGeneration); err != nil {
				return zero, err
			}
			if armLease != nil {
				armLease.LeaseGeneration = e.HandlerLeaseGeneration
				if err := insertHandlerArmAssignment(ctx, tx, task, e, *armLease); err != nil {
					return zero, err
				}
				e.HandlerArm = armLease
			}
		case "freeze":
			if e.State != "launching" || len(e.LaunchJSON) != 0 || !json.Valid(req.LaunchJSON) || len(req.LaunchJSON) == 0 {
				return zero, api.ErrConflict
			}
			if e.HandlerID == "" || e.HandlerRunID == "" || e.HandlerLeaseGeneration < 1 {
				return zero, fmt.Errorf("%w: exact handler lease missing", api.ErrConflict)
			}
			if err := requireCurrentConfirmedTeamOrder(ctx, tx, task, e.ItemID, e.ItemRevision, e.OrderMessageSeq); err != nil {
				return zero, err
			}
			var plan struct {
				Task                   string          `json:"task"`
				Item                   string          `json:"item"`
				Revision               int64           `json:"revision"`
				Order                  int64           `json:"order"`
				HandlerID              string          `json:"handlerId"`
				HandlerRunID           string          `json:"handlerRunId"`
				HandlerLeaseGeneration int64           `json:"handlerLeaseGeneration"`
				Context                json.RawMessage `json:"context"`
				Members                []struct {
					Fields struct {
						AgentID string `json:"agentId"`
						Name    string `json:"name"`
						Cwd     string `json:"cwd"`
					} `json:"fields"`
					State string `json:"state"`
					RunID string `json:"runId"`
				} `json:"members"`
			}
			if json.Unmarshal(req.LaunchJSON, &plan) != nil || plan.Task != task || plan.Item != e.ItemID || plan.Revision != e.ItemRevision || plan.Order != e.OrderMessageSeq || len(plan.Context) == 0 || !json.Valid(plan.Context) || len(plan.Members) == 0 {
				return zero, api.ErrInvalid
			}
			limit, err := queueConcurrencyLimit(ctx, tx, task)
			if err != nil {
				return zero, err
			}
			if (queueParallel(limit) || plan.HandlerID != "") && (plan.HandlerID != e.HandlerID || plan.HandlerRunID != e.HandlerRunID || plan.HandlerLeaseGeneration != e.HandlerLeaseGeneration) {
				return zero, fmt.Errorf("%w: frozen handler lease differs from queue claim", api.ErrConflict)
			}
			seenAgents := map[string]bool{}
			seenRuns := map[string]bool{}
			for _, member := range plan.Members {
				if !api.ValidID(member.Fields.AgentID, "agt") || !validRunID(member.RunID) || member.Fields.Name == "" || member.Fields.Cwd == "" || member.State != "unstarted" || seenAgents[member.Fields.AgentID] || seenRuns[member.RunID] {
					return zero, api.ErrInvalid
				}
				seenAgents[member.Fields.AgentID] = true
				seenRuns[member.RunID] = true
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,state) VALUES(?,?,?,?,'launching')`, task, e.ItemID, plan.Members[0].Fields.AgentID, plan.Members[0].RunID); err != nil {
				return zero, fmt.Errorf("%w: item lead is already reserved", api.ErrConflict)
			}
			e.LaunchJSON = req.LaunchJSON
		case "attempt", "started":
			if e.State != "launching" || len(e.LaunchJSON) == 0 {
				return zero, api.ErrConflict
			}
			var plan map[string]any
			if json.Unmarshal(e.LaunchJSON, &plan) != nil {
				return zero, api.ErrConflict
			}
			members, ok := plan["members"].([]any)
			if !ok || req.MemberIndex < 0 || req.MemberIndex >= len(members) {
				return zero, api.ErrInvalid
			}
			member, ok := members[req.MemberIndex].(map[string]any)
			if !ok {
				return zero, api.ErrConflict
			}
			if req.Operation == "attempt" {
				if err := requireCurrentConfirmedTeamOrder(ctx, tx, task, e.ItemID, e.ItemRevision, e.OrderMessageSeq); err != nil {
					return zero, err
				}
				limit, err := queueConcurrencyLimit(ctx, tx, task)
				if err != nil {
					return zero, err
				}
				// A launch already under way keeps its members; the disk
				// reserve gates only new admission.
				if queueParallel(limit) {
					if err := checkTeamHostCapacity(ctx, tx, e.Host, s.now(), 0); err != nil {
						return zero, err
					}
				}
				run, _ := member["runId"].(string)
				if member["state"] != "unstarted" || !validRunID(run) {
					return zero, api.ErrConflict
				}
				member["state"] = "uncertain"
			} else {
				if member["state"] != "uncertain" || !validRunID(req.MemberRunID) || member["runId"] != req.MemberRunID {
					return zero, api.ErrConflict
				}
				member["state"] = "started"
				member["runId"] = req.MemberRunID
			}
			e.LaunchJSON, _ = json.Marshal(plan)
		case "running":
			if e.State != "launching" || len(e.LaunchJSON) == 0 {
				return zero, api.ErrConflict
			}
			var reserved string
			var generation int64
			if err := tx.QueryRowContext(ctx, `SELECT entry_id,pause_generation FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, e.ID).Scan(&reserved, &generation); err != nil {
				return zero, fmt.Errorf("%w: launch reservation is missing", api.ErrConflict)
			}
			if reserved != e.ID || generation != e.PauseGeneration || t.PauseGeneration != e.PauseGeneration || t.PauseState != api.ProjectPauseActive {
				return zero, fmt.Errorf("%w: launch reservation or pause changed", api.ErrConflict)
			}
			var plan struct {
				Members []struct {
					State string `json:"state"`
					RunID string `json:"runId"`
				} `json:"members"`
			}
			if json.Unmarshal(e.LaunchJSON, &plan) != nil || len(plan.Members) == 0 {
				return zero, api.ErrConflict
			}
			for _, m := range plan.Members {
				if m.State != "started" || m.RunID == "" {
					return zero, api.ErrConflict
				}
			}
			e.State = "running"
			if _, err := tx.ExecContext(ctx, `UPDATE team_launch_reservations SET state='running' WHERE task_id=? AND entry_id=?`, task, e.ID); err != nil {
				return zero, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE item_team_leads SET state='running' WHERE task_id=? AND item_id=?`, task, e.ItemID); err != nil {
				return zero, err
			}
		case "replace_lead":
			if e.State != "running" || len(e.CloseJSON) != 0 || !api.ValidID(req.LeadAgentID, "agt") || !validRunID(req.LeadRunID) || req.ExpectedLeadRevision < 1 {
				return zero, api.ErrConflict
			}
			var oldAgent, oldRun, oldState string
			var oldRevision int64
			if err := tx.QueryRowContext(ctx, `SELECT agent_id,run_id,revision,state FROM item_team_leads WHERE task_id=? AND item_id=?`, task, e.ItemID).Scan(&oldAgent, &oldRun, &oldRevision, &oldState); err != nil || oldState != "running" || oldRevision != req.ExpectedLeadRevision || (oldAgent == req.LeadAgentID && oldRun == req.LeadRunID) {
				return zero, fmt.Errorf("%w: exact item lead changed", api.ErrConflict)
			}
			candidate, candidateErr := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND run_id=?`, task, req.LeadAgentID, req.LeadRunID))
			var boundItem string
			var boundRevision int64
			bindingErr := tx.QueryRowContext(ctx, `SELECT item_id,item_revision FROM agent_work_item_bindings WHERE agent_id=? AND run_id=? AND item_task_id=?`, req.LeadAgentID, req.LeadRunID, task).Scan(&boundItem, &boundRevision)
			if candidateErr != nil || bindingErr != nil || candidate.Role != "" || !candidate.Online || candidate.Status == api.AgentRetired || candidate.Status == api.AgentClosed || candidate.Status == api.AgentExited || boundItem != e.ItemID || boundRevision != e.ItemRevision {
				return zero, fmt.Errorf("%w: replacement must be a live exact member of this item", api.ErrConflict)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE item_team_leads SET agent_id=?,run_id=?,revision=revision+1 WHERE task_id=? AND item_id=? AND agent_id=? AND run_id=? AND revision=? AND state='running'`, req.LeadAgentID, req.LeadRunID, task, e.ItemID, oldAgent, oldRun, oldRevision); err != nil {
				return zero, err
			}
			previous, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND run_id=?`, task, oldAgent, oldRun))
			if err != nil {
				return zero, err
			}
			if err := s.handOffRoleObligations(ctx, tx, t, previous, candidate); err != nil {
				return zero, err
			}
			if strings.EqualFold(t.Orchestrator, previous.Name) {
				result, err := tx.ExecContext(ctx, `UPDATE tasks SET orchestrator=?,lead_revision=lead_revision+1 WHERE id=? AND orchestrator=? AND lead_revision=?`, candidate.Name, task, t.Orchestrator, t.LeadRevision)
				if err != nil {
					return zero, err
				}
				if count, _ := result.RowsAffected(); count != 1 {
					return zero, fmt.Errorf("%w: serial project lead changed", api.ErrConflict)
				}
			}
		case "close":
			if !queueClosable(e) || len(e.CloseJSON) != 0 || !json.Valid(req.CloseJSON) || len(req.CloseJSON) == 0 {
				return zero, api.ErrConflict
			}
			e.CloseJSON = req.CloseJSON
		case "close_refresh":
			if !queueClosable(e) || len(e.CloseJSON) == 0 {
				return zero, api.ErrConflict
			}
			var previous api.TeamCloseRequest
			if json.Unmarshal(e.CloseJSON, &previous) != nil || previous.RequestID != req.CloseRequestID {
				return zero, api.ErrConflict
			}
			var receipt int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_close_receipts WHERE task_id=? AND request_id=?`, task, previous.RequestID).Scan(&receipt); err != nil {
				return zero, err
			}
			if receipt != 0 {
				return zero, fmt.Errorf("%w: exact team close already has a receipt", api.ErrConflict)
			}
			e.CloseJSON = nil
		case "accept":
			if req.Acceptance == nil {
				return zero, fmt.Errorf("%w: exact active handler and unaccepted team are required", api.ErrConflict)
			}
			if (e.State == "running" || e.State == "finished") && e.Acceptance != nil && req.HandlerAgentID == e.HandlerID && req.HandlerRunID == e.HandlerRunID && sameTeamAcceptance(*e.Acceptance, *req.Acceptance) {
				// Recovery re-accept of the saved tuple (for example after the
				// handler's done save already recorded it) changes nothing: no
				// new revision and no second release job.
				return e, nil
			}
			if err = acceptTeamQueueEntry(ctx, tx, task, &e, *req.Acceptance, req.HandlerAgentID, req.HandlerRunID, now); err != nil {
				return zero, err
			}
		case "finish":
			if !queueClosable(e) || len(e.CloseJSON) == 0 {
				return zero, api.ErrConflict
			}
			// An owner-integrated entry gave up its reservation when it was
			// released; its close and cleanup receipts are still required.
			ownerIntegrated := e.OwnerIntegration != nil
			var reserved string
			if err := tx.QueryRowContext(ctx, `SELECT entry_id FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, e.ID).Scan(&reserved); !ownerIntegrated && (err != nil || reserved != e.ID) {
				return zero, fmt.Errorf("%w: exact queue reservation is missing", api.ErrConflict)
			}
			var closeReq api.TeamCloseRequest
			if json.Unmarshal(e.CloseJSON, &closeReq) != nil || closeReq.ItemID != e.ItemID || !validRequestID(closeReq.RequestID) {
				return zero, api.ErrConflict
			}
			var receiptJSON string
			err = tx.QueryRowContext(ctx, `SELECT result_json FROM team_close_receipts WHERE task_id=? AND request_id=?`, task, closeReq.RequestID).Scan(&receiptJSON)
			if errors.Is(err, sql.ErrNoRows) {
				return zero, fmt.Errorf("%w: exact team close receipt is pending", api.ErrConflict)
			}
			if err != nil {
				return zero, err
			}
			var result api.TeamCloseResult
			var leadState string
			leadErr := tx.QueryRowContext(ctx, `SELECT state FROM item_team_leads WHERE task_id=? AND item_id=?`, task, e.ItemID).Scan(&leadState)
			if leadErr != nil && !errors.Is(leadErr, sql.ErrNoRows) {
				return zero, leadErr
			}
			if json.Unmarshal([]byte(receiptJSON), &result) != nil || result.TaskID != task || result.ItemID != e.ItemID || len(result.Members) == 0 || (leadErr == nil && leadState != "closed") || (errors.Is(leadErr, sql.ErrNoRows) && t.Orchestrator != "") {
				return zero, fmt.Errorf("%w: exact team close receipt is pending", api.ErrConflict)
			}
			for _, member := range result.Members {
				var status string
				var done bool
				err = tx.QueryRowContext(ctx, `SELECT status,cleanup_done FROM agents WHERE task_id=? AND id=? AND run_id=?`, task, member.AgentID, member.RunID).Scan(&status, &done)
				if errors.Is(err, sql.ErrNoRows) {
					return zero, fmt.Errorf("%w: exact team run disappeared", api.ErrConflict)
				}
				if err != nil {
					return zero, err
				}
				if status != api.AgentClosed || !done {
					return zero, fmt.Errorf("%w: exact team cleanup receipts are pending", api.ErrConflict)
				}
			}
			item, err := getWorkItem(tx, ctx, task, e.ItemID)
			if err != nil {
				return zero, err
			}
			if item.Status != "done" && item.Status != "dismissed" {
				return zero, fmt.Errorf("%w: item is not terminal", api.ErrConflict)
			}
			if ownerIntegrated && req.Integration != nil {
				return zero, fmt.Errorf("%w: the owner integrated this entry; finish it without an integration snapshot", api.ErrInvalid)
			}
			if !ownerIntegrated && item.Status == "done" && e.Repository != "" && (req.Integration == nil || e.Acceptance == nil) {
				return zero, fmt.Errorf("%w: exact handler acceptance receipt is required", api.ErrConflict)
			}
			if req.Integration != nil {
				candidate := *req.Integration
				if item.Status != "done" || e.Repository == "" || e.Acceptance == nil || e.Acceptance.ItemRevision != item.Revision || !reflect.DeepEqual(e.Acceptance.CompletionReport, item.CompletionReport) || candidate.Repository != e.Acceptance.Repository || candidate.BaseCommit != e.Acceptance.BaseCommit || candidate.Worktree != e.Acceptance.Worktree || candidate.Branch != e.Acceptance.Branch || candidate.Commit != e.Acceptance.Commit || candidate.Evidence != e.Acceptance.Evidence {
					return zero, api.ErrInvalid
				}
				candidate.ReadyAt = now
				e.Integration = &candidate
			}
			e.State = "finished"
			_, err = tx.ExecContext(ctx, `DELETE FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, e.ID)
			if err != nil {
				return zero, err
			}
			if err = finishHandlerArmAssignment(ctx, tx, e, now); err != nil {
				return zero, err
			}
		case "fail":
			if e.State != "queued" && e.State != "launching" && e.State != "running" || strings.TrimSpace(req.Failure) == "" {
				return zero, api.ErrConflict
			}
			e.State = "failed"
			cause := []rune(req.Failure)
			if len(cause) > 1000 {
				cause = cause[:1000]
			}
			e.Failure = string(cause)
			notice := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Team queue failed and requires owner action", Refs: map[string]string{"escalation": "owner", "item": e.ItemID, "entry": e.ID}, Body: api.EnvelopeBody{Text: e.Failure + ". Automatic retry is disabled; reconcile this entry before more launches."}}
			message, insertErr := s.insertMessage(ctx, tx, t, api.PostMessageRequest{Envelope: &notice}, api.Agent{}, api.Caller{Node: "team_queue", User: "runner"}, false, false)
			if insertErr != nil {
				return zero, insertErr
			}
			e.EscalationSeq = message.Seq
		}
		if req.Operation != "remove" && req.Operation != "reorder" {
			e.Revision++
			acceptanceJSON := ""
			if e.Acceptance != nil {
				data, _ := json.Marshal(e.Acceptance)
				acceptanceJSON = string(data)
			}
			integrationJSON := ""
			if e.Integration != nil {
				data, _ := json.Marshal(e.Integration)
				integrationJSON = string(data)
			}
			ownerIntegrationJSON := ""
			if e.OwnerIntegration != nil {
				data, _ := json.Marshal(e.OwnerIntegration)
				ownerIntegrationJSON = string(data)
			}
			ownedJSON, _ := json.Marshal(e.Ownership)
			_, err = tx.ExecContext(ctx, `UPDATE team_queue_entries SET state=?,revision=?,pause_generation=?,launch_json=?,close_json=?,failure=?,escalation_seq=?,released_at=?,handler_id=?,handler_run_id=?,handler_lease_generation=?,acceptance_json=?,integration_json=?,base_commit=?,ownership_json=?,owner_integration_json=?,serial=?,cwd=?,updated_at=? WHERE id=?`, e.State, e.Revision, e.PauseGeneration, string(e.LaunchJSON), string(e.CloseJSON), e.Failure, e.EscalationSeq, e.ReleasedAt, e.HandlerID, e.HandlerRunID, e.HandlerLeaseGeneration, acceptanceJSON, integrationJSON, e.BaseCommit, string(ownedJSON), ownerIntegrationJSON, e.Serial, e.Cwd, now, e.ID)
			if err != nil {
				return zero, err
			}
			e.UpdatedAt = now
		}
	default:
		return zero, api.ErrInvalid
	}
	// Acceptance with a native exact-SHA receipt durably enqueues delivery in
	// the same transaction. Legacy acceptance remains visibly unreleased.
	if req.Operation == "accept" {
		if err = enqueueAcceptedRelease(ctx, tx, t, &e); err != nil {
			return zero, err
		}
	}
	if req.Operation == "reorder" {
		e, err = scanTeamQueue(tx.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE id=?`, e.ID))
		if err != nil {
			return zero, err
		}
	}
	encoded, _ := json.Marshal(e)
	if _, err = tx.ExecContext(ctx, `INSERT INTO team_queue_requests(task_id,request_id,payload_hash,result_json) VALUES(?,?,?,?)`, task, req.RequestID, hash, encoded); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return e, nil
}

// queueScopeAuthority checks who may declare an entry's ownership. A request
// without an agent is the owner's unbound CLI. A database handler names its
// exact live run: any handler may scope a queued entry, and only the leased
// handler an admitted one. The item's current lead may scope its own admitted
// entry. Like accept, the named run is checked against the hub's records.
func queueScopeAuthority(ctx context.Context, tx *sql.Tx, task string, e api.TeamQueueEntry, req api.TeamQueueRequest) error {
	refused := fmt.Errorf("%w: only the owner, a database handler or the entry's item lead may scope it", api.ErrConflict)
	live := func(agentID, runID, role string) bool {
		var actualRole, actualRun, status string
		err := tx.QueryRowContext(ctx, `SELECT role,run_id,status FROM agents WHERE task_id=? AND id=?`, task, agentID).Scan(&actualRole, &actualRun, &status)
		return err == nil && actualRole == role && actualRun == runID && status != api.AgentClosed && status != api.AgentExited && status != api.AgentRetired
	}
	handler := req.HandlerAgentID != "" || req.HandlerRunID != ""
	lead := req.LeadAgentID != "" || req.LeadRunID != ""
	switch {
	case !handler && !lead:
		return nil
	case handler && lead:
		return api.ErrInvalid
	case handler:
		if !live(req.HandlerAgentID, req.HandlerRunID, api.AgentRoleDatabaseHandler) {
			return refused
		}
		if e.State != "queued" && (req.HandlerAgentID != e.HandlerID || req.HandlerRunID != e.HandlerRunID) {
			return refused
		}
		return nil
	default:
		if e.State == "queued" || !live(req.LeadAgentID, req.LeadRunID, "") {
			return refused
		}
		var agentID, runID, state string
		if err := tx.QueryRowContext(ctx, `SELECT agent_id,run_id,state FROM item_team_leads WHERE task_id=? AND item_id=?`, task, e.ItemID).Scan(&agentID, &runID, &state); err != nil || state == "closed" || agentID != req.LeadAgentID || runID != req.LeadRunID {
			return refused
		}
		return nil
	}
}

// verifiedRepository reports whether a verification plan's repository names the
// accepted candidate's repository: the same .git directory, that repository's
// root (the directory holding .git), or the exact candidate worktree.
func verifiedRepository(planRepository string, a api.TeamIntegrationAcceptance) bool {
	plan := filepath.Clean(planRepository)
	repository := filepath.Clean(a.Repository)
	if plan == repository || plan == filepath.Clean(a.Worktree) {
		return true
	}
	return filepath.Base(repository) == ".git" && plan == filepath.Dir(repository)
}

// acceptTeamQueueEntry records the exact handler's integration acceptance on a
// running, repository-backed entry. Both the queue accept operation and the
// handler's done save use it, so they refuse exactly the same candidates.
func acceptTeamQueueEntry(ctx context.Context, tx *sql.Tx, task string, e *api.TeamQueueEntry, candidate api.TeamIntegrationAcceptance, handlerAgent, handlerRun, now string) error {
	// The owner's integration record replaces handler acceptance: accepting
	// the released entry would record a second candidate and could enqueue a
	// second release job.
	if e.OwnerIntegration != nil {
		return fmt.Errorf("%w: entry %s was integrated by the owner at %s; it takes no handler acceptance", api.ErrConflict, e.ID, e.OwnerIntegration.Commit)
	}
	if e.ReleasedAt != "" {
		return fmt.Errorf("%w: entry %s was released; it takes no handler acceptance", api.ErrConflict, e.ID)
	}
	if e.State != "running" || e.Acceptance != nil || e.Repository == "" || handlerAgent != e.HandlerID || handlerRun != e.HandlerRunID {
		return fmt.Errorf("%w: exact active handler and unaccepted team are required", api.ErrConflict)
	}
	var handlerRole, handlerStatus, handlerRunID string
	if err := tx.QueryRowContext(ctx, `SELECT role,status,run_id FROM agents WHERE task_id=? AND id=?`, task, handlerAgent).Scan(&handlerRole, &handlerStatus, &handlerRunID); err != nil || handlerRole != api.AgentRoleDatabaseHandler || handlerRunID != handlerRun || handlerStatus == api.AgentClosed || handlerStatus == api.AgentExited || handlerStatus == api.AgentRetired {
		return fmt.Errorf("%w: assigned handler run is unavailable", api.ErrConflict)
	}
	item, err := getWorkItem(tx, ctx, task, e.ItemID)
	if err != nil {
		return err
	}
	records, err := verificationRecords(ctx, tx, task, item.ID)
	if err != nil {
		return err
	}
	verificationPlan, _ := currentVerification(records)
	required, err := verificationRequired(ctx, tx, task, item.ID)
	if err != nil {
		return err
	}
	// A verified team is accepted on its verified base, which is the
	// tasks-hub tip at launch rather than the queue-time base. Its plan
	// may name the repository (its .git directory or its root) or the
	// exact candidate worktree, which tt has already resolved to this
	// entry's repository.
	if required && (verificationPlan == nil || !verifiedRepository(verificationPlan.Repository, candidate) || (candidate.BaseCommit != e.BaseCommit && candidate.BaseCommit != verificationPlan.BaseCommit)) {
		return verificationConflict("acceptance repository/base mismatch")
	}
	base := e.BaseCommit
	if required {
		base = verificationPlan.BaseCommit
		candidate.BaseCommit = base
	}
	if err = reviewCompletion(ctx, tx, item, candidate.Commit); err != nil {
		return err
	}
	completion, err := checkVerificationCompletion(ctx, tx, item, candidate.Commit)
	if err != nil {
		return err
	}
	if candidate.ResolvedKnownFailures != nil || item.Status != "done" || candidate.ItemRevision != item.Revision || !reflect.DeepEqual(candidate.CompletionReport, item.CompletionReport) || candidate.Repository != e.Repository || candidate.BaseCommit != base || !filepath.IsAbs(candidate.Worktree) || filepath.Clean(candidate.Worktree) != candidate.Worktree || strings.ContainsRune(candidate.Worktree, '\x00') || !validGitCommit(candidate.Commit) || candidate.Branch == "" || len(candidate.Branch) > 200 || strings.ContainsAny(candidate.Branch, "\x00\n\r") || strings.TrimSpace(candidate.Evidence) == "" || candidate.AcceptedAt != "" {
		return api.ErrInvalid
	}
	candidate.AcceptedAt = now
	if completion != nil && len(completion.Resolved) > 0 {
		candidate.ResolvedKnownFailures = &api.ResolvedKnownFailures{ReceiptGeneration: completion.ReceiptGeneration, ReceiptDigest: verificationDigest(completion.Receipt), MatrixDigest: completion.Plan.MatrixDigest, Entries: completion.Resolved}
	}
	e.Acceptance = &candidate
	e.BaseCommit = base
	return nil
}

// enqueueAcceptedRelease durably enqueues delivery for an acceptance with a
// native exact-SHA receipt, in the accepting transaction. The entry's
// acceptance must already be written. Legacy acceptance remains visibly
// unreleased.
func enqueueAcceptedRelease(ctx context.Context, tx *sql.Tx, t api.Task, e *api.TeamQueueEntry) error {
	records, err := verificationRecords(ctx, tx, t.ID, e.ItemID)
	if err != nil {
		return err
	}
	plan, receipt := currentVerification(records)
	if plan == nil || receipt == nil {
		return nil
	}
	_, p, r, err := releaseCandidate(ctx, tx, t.ID, e.ID)
	if err != nil {
		return err
	}
	j := api.ReleaseJob{ID: api.NewID("rel"), TaskID: t.ID, EntryID: e.ID, ItemID: e.ItemID, ItemRevision: e.Acceptance.ItemRevision, ScopeRevision: p.ScopeRevision, OrderMessageSeq: p.OrderMessageSeq, Repository: p.Repository, BaseCommit: p.BaseCommit, Commit: p.Commit, VerificationDigest: verificationDigest(r), Plan: p, State: "verified", Generation: 1, PauseGeneration: t.PauseGeneration}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_jobs VALUES(?,?,?,?,?,?)`, t.ID, j.ID, e.ID, j.State, j.Generation, string(b)); err != nil {
		return err
	}
	e.Release = &j
	return nil
}

// sameTeamAcceptance reports whether a submitted acceptance names the saved
// one. Evidence text and the acceptance time are not part of the identity.
func sameTeamAcceptance(saved, submitted api.TeamIntegrationAcceptance) bool {
	return saved.Repository == submitted.Repository && saved.BaseCommit == submitted.BaseCommit && saved.Worktree == submitted.Worktree && saved.Branch == submitted.Branch && saved.Commit == submitted.Commit && saved.ItemRevision == submitted.ItemRevision && reflect.DeepEqual(saved.CompletionReport, submitted.CompletionReport)
}

// pendingTeamQueueAcceptance returns the item's running, repository-backed team
// queue entry that still waits on handler acceptance, or nil.
func pendingTeamQueueAcceptance(ctx context.Context, tx *sql.Tx, task, item string) (*api.TeamQueueEntry, error) {
	e, err := scanTeamQueue(tx.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND item_id=? AND state='running' AND released_at='' AND repository<>'' AND acceptance_json='' ORDER BY position LIMIT 1`, task, item))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// doneSaveQueueGate checks a done save against the item's team queue before
// any write. A save that carries an acceptance must name the entry waiting on
// it. A database handler's save without one is refused while an entry waits,
// so the queue can no longer stall on a separate remembered accept. Owner and
// UI saves are unchanged; tt team queue accept stays their recovery path.
func doneSaveQueueGate(ctx context.Context, tx *sql.Tx, task, item, agentID string, accept *api.WorkItemQueueAcceptance) (*api.TeamQueueEntry, error) {
	pending, err := pendingTeamQueueAcceptance(ctx, tx, task, item)
	if err != nil {
		return nil, err
	}
	if accept != nil {
		var integratedID, integratedJSON string
		lookupErr := tx.QueryRowContext(ctx, `SELECT id,owner_integration_json FROM team_queue_entries WHERE task_id=? AND item_id=? AND owner_integration_json<>'' ORDER BY position DESC LIMIT 1`, task, item).Scan(&integratedID, &integratedJSON)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return nil, lookupErr
		}
		if lookupErr == nil {
			var record api.TeamQueueOwnerIntegration
			_ = json.Unmarshal([]byte(integratedJSON), &record)
			return nil, fmt.Errorf("%w: entry %s was integrated by the owner at %s; save done without --worktree/--branch/--commit", api.ErrConflict, integratedID, record.Commit)
		}
		if pending == nil || pending.ID != accept.EntryID {
			return nil, fmt.Errorf("%w: no running team queue entry waits on this acceptance", api.ErrConflict)
		}
		return pending, nil
	}
	if pending == nil || agentID == "" {
		return nil, nil
	}
	var role string
	if err = tx.QueryRowContext(ctx, `SELECT role FROM agents WHERE task_id=? AND id=?`, task, agentID).Scan(&role); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if role == api.AgentRoleDatabaseHandler {
		return nil, fmt.Errorf("%w: team queue entry %s waits on acceptance; save done with the accepted --worktree, --branch and --commit", api.ErrConflict, pending.ID)
	}
	return nil, nil
}

// acceptTeamQueueOnDoneSave records the queue acceptance inside the handler's
// done save, after the item row and its update receipt are written. Any
// refusal rolls back the whole save, so the item stays open.
func acceptTeamQueueOnDoneSave(ctx context.Context, tx *sql.Tx, t api.Task, e *api.TeamQueueEntry, accept api.WorkItemQueueAcceptance, agentID, runID, receiptID, now string) error {
	item, err := getWorkItem(tx, ctx, t.ID, e.ItemID)
	if err != nil {
		return err
	}
	evidence := accept.Evidence
	if evidence == "" {
		evidence = fmt.Sprintf("handler-saved completion receipt %s revision %d", receiptID, item.Revision)
	}
	candidate := api.TeamIntegrationAcceptance{Repository: e.Repository, BaseCommit: e.BaseCommit, Worktree: accept.Worktree, Branch: accept.Branch, Commit: accept.Commit, ItemRevision: item.Revision, CompletionReport: item.CompletionReport, Evidence: evidence}
	prior := e.Revision
	if err = acceptTeamQueueEntry(ctx, tx, t.ID, e, candidate, agentID, runID, now); err != nil {
		return err
	}
	e.Revision++
	data, err := json.Marshal(e.Acceptance)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE team_queue_entries SET revision=?,acceptance_json=?,base_commit=?,updated_at=? WHERE task_id=? AND id=? AND revision=?`, e.Revision, string(data), e.BaseCommit, now, t.ID, e.ID, prior)
	if err != nil {
		return err
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
		return fmt.Errorf("%w: entry revision changed", api.ErrConflict)
	}
	return enqueueAcceptedRelease(ctx, tx, t, e)
}
