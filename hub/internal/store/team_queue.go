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
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS team_queue_entries (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), item_id TEXT NOT NULL REFERENCES work_items(id),
 item_revision INTEGER NOT NULL, order_seq INTEGER NOT NULL, template TEXT NOT NULL,
 position INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('queued','launching','running','finished','failed')),
 revision INTEGER NOT NULL DEFAULT 1, host TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '',
 pause_generation INTEGER NOT NULL DEFAULT 0, launch_json BLOB NOT NULL DEFAULT '', close_json BLOB NOT NULL DEFAULT '',
 failure TEXT NOT NULL DEFAULT '', escalation_seq INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(task_id,item_id));
 CREATE INDEX IF NOT EXISTS team_queue_order ON team_queue_entries(task_id,position);
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
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS team_queue_settings(task_id TEXT PRIMARY KEY REFERENCES tasks(id), concurrency_limit INTEGER NOT NULL DEFAULT 1 CHECK(concurrency_limit BETWEEN 1 AND 2));
		CREATE TABLE IF NOT EXISTS team_host_policies(host TEXT PRIMARY KEY,version INTEGER NOT NULL,expires_at TEXT NOT NULL,max_sessions INTEGER NOT NULL,max_polling INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS team_host_usage(host TEXT PRIMARY KEY,limiter_domain TEXT NOT NULL,policy_version INTEGER NOT NULL DEFAULT 0,observed_at TEXT NOT NULL,relay_bindings INTEGER NOT NULL,complete INTEGER NOT NULL,source_digest TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS item_team_leads(task_id TEXT NOT NULL,item_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,revision INTEGER NOT NULL DEFAULT 1,state TEXT NOT NULL CHECK(state IN ('launching','running','closed')),PRIMARY KEY(task_id,item_id));
		DROP INDEX IF EXISTS team_queue_active;
		CREATE INDEX IF NOT EXISTS team_queue_active ON team_queue_entries(task_id,state) WHERE state IN ('launching','running');`)
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
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
	var usageVersion int
	if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('team_host_usage') WHERE name='policy_version'`).Scan(&usageVersion); err != nil {
		return err
	}
	if usageVersion == 0 {
		if _, err := tx.Exec(`ALTER TABLE team_host_usage ADD COLUMN policy_version INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
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

const teamQueueCols = `id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,close_json,failure,escalation_seq,released_at,repository,ownership_json,handler_id,handler_run_id,handler_lease_generation,base_commit,acceptance_json,integration_json`

func scanTeamQueue(row interface{ Scan(...any) error }) (api.TeamQueueEntry, error) {
	var e api.TeamQueueEntry
	var launch, close []byte
	var ownership, acceptance, integration string
	err := row.Scan(&e.ID, &e.TaskID, &e.ItemID, &e.ItemRevision, &e.OrderMessageSeq, &e.Template, &e.Position, &e.State, &e.Revision, &e.Host, &e.Cwd, &e.PauseGeneration, &launch, &close, &e.Failure, &e.EscalationSeq, &e.ReleasedAt, &e.Repository, &ownership, &e.HandlerID, &e.HandlerRunID, &e.HandlerLeaseGeneration, &e.BaseCommit, &acceptance, &integration)
	if err != nil {
		return e, err
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

func (s *Store) ListTeamQueue(ctx context.Context, task string) (api.TeamQueueList, error) {
	if !api.ValidID(task, "tsk") {
		return api.TeamQueueList{}, api.ErrInvalid
	}
	out := api.TeamQueueList{Entries: []api.TeamQueueEntry{}}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&out.ConcurrencyLimit); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? ORDER BY position`, task)
	if err != nil {
		return api.TeamQueueList{}, err
	}
	defer rows.Close()
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
	activeCount := 0
	for i, entry := range out.Entries {
		if err := s.loadTeamActivities(ctx, &out.Entries[i]); err != nil {
			return out, err
		}
		if entry.State == "launching" || entry.State == "running" || (entry.State == "failed" && entry.ReleasedAt == "") {
			activeCount++
		}
		if entry.State == "running" && entry.Repository != "" && entry.Acceptance == nil {
			var status string
			if err := s.db.QueryRowContext(ctx, `SELECT status FROM work_items WHERE task_id=? AND id=?`, task, entry.ItemID).Scan(&status); err != nil {
				return out, err
			}
			if status == "done" {
				out.Entries[i].BlockReason = "Waiting for handler acceptance"
			}
		}
	}
	var capacityTx *sql.Tx
	if out.ConcurrencyLimit > 1 {
		capacityTx, err = s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return out, err
		}
		defer capacityTx.Rollback()
	}
	for i := range out.Entries {
		if out.Entries[i].State != "queued" {
			continue
		}
		if activeCount >= out.ConcurrencyLimit {
			out.Entries[i].BlockReason = "All team slots are reserved"
		} else if capacityTx != nil {
			if capacityErr := checkTeamHostCapacity(ctx, capacityTx, out.Entries[i].Host, s.now(), 1); capacityErr != nil {
				out.Entries[i].BlockReason = capacityErr.Error()
			}
		}
		for j := range out.Entries {
			if i == j {
				continue
			}
			other := out.Entries[j]
			if other.State != "launching" && other.State != "running" && !(other.State == "failed" && other.ReleasedAt == "") {
				continue
			}
			if queueEntryConflicts(out.Entries[i], other) || (out.Entries[i].Cwd != "" && out.Entries[i].Cwd == other.Cwd) {
				out.Entries[i].BlockedBy = append(out.Entries[i].BlockedBy, other.ID)
			}
		}
	}
	return out, nil
}

func (s *Store) TeamQueuesByHost(ctx context.Context, host string) (api.TeamQueueList, error) {
	if host == "" {
		return api.TeamQueueList{}, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE host=? AND state IN ('queued','launching','running') ORDER BY task_id,position LIMIT 200`, host)
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
		if err := s.loadTeamActivities(ctx, &out.Entries[i]); err != nil {
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

func (s *Store) GetTeamQueueEntry(ctx context.Context, task, id string) (api.TeamQueueEntry, error) {
	if !api.ValidID(task, "tsk") || !validTeamQueueID(id) {
		return api.TeamQueueEntry{}, api.ErrInvalid
	}
	e, err := scanTeamQueue(s.db.QueryRowContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND id=?`, task, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, api.ErrNotFound
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
	if identity.Operation == "release" || identity.Operation == "accept" {
		// A lost response is replayable after the release increments revision.
		identity.ExpectedRevision = 0
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	now := ts(s.now())
	var e api.TeamQueueEntry
	switch req.Operation {
	case "set_host_policy":
		if req.Host == "" || !validLimiterDomain(req.LimiterDomain) || len(req.LimiterDomain) > 255 || req.HostPolicyVersion < 1 || req.HostMaxSessions < 1 || req.HostMaxPolling < 1 || req.HostMaxRelayBindings < 1 || req.HostMaxRequestsPerMinute < 1 || req.HostMaxBurst < 1 || req.HostHeadroomPercent < 1 || req.HostHeadroomPercent >= 100 || req.HostMaxRequestsPerMinute > 1000000 || req.HostMaxBurst > 100000 {
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_host_policies(host,version,expires_at,max_sessions,max_polling,limiter_domain,max_relay_bindings,max_requests_per_minute,max_burst,headroom_percent) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(host) DO UPDATE SET version=excluded.version,expires_at=excluded.expires_at,max_sessions=excluded.max_sessions,max_polling=excluded.max_polling,limiter_domain=excluded.limiter_domain,max_relay_bindings=excluded.max_relay_bindings,max_requests_per_minute=excluded.max_requests_per_minute,max_burst=excluded.max_burst,headroom_percent=excluded.headroom_percent`, req.Host, req.HostPolicyVersion, req.HostPolicyExpires, req.HostMaxSessions, req.HostMaxPolling, req.LimiterDomain, req.HostMaxRelayBindings, req.HostMaxRequestsPerMinute, req.HostMaxBurst, req.HostHeadroomPercent); err != nil {
			return zero, err
		}
		e = api.TeamQueueEntry{TaskID: task, Host: req.Host, State: "host_policy", Revision: req.HostPolicyVersion}
	case "observe_host":
		u := req.HostUsage
		if req.Host == "" || u == nil || u.Host != req.Host || !validLimiterDomain(u.LimiterDomain) || u.PolicyVersion < 1 || u.RelayBindings < 0 || !validContextDigest(u.SourceDigest) {
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
			if parseErr != nil || !observed.After(previousAt) && (observed.Before(previousAt) || previous.SourceDigest != u.SourceDigest || previous.Complete != u.Complete || previous.RelayBindings != u.RelayBindings) {
				return zero, fmt.Errorf("%w: host usage observation regressed", api.ErrConflict)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO team_host_usage(host,limiter_domain,policy_version,observed_at,relay_bindings,complete,source_digest) VALUES(?,?,?,?,?,?,?) ON CONFLICT(host) DO UPDATE SET limiter_domain=excluded.limiter_domain,policy_version=excluded.policy_version,observed_at=excluded.observed_at,relay_bindings=excluded.relay_bindings,complete=excluded.complete,source_digest=excluded.source_digest`, u.Host, u.LimiterDomain, u.PolicyVersion, u.ObservedAt, u.RelayBindings, u.Complete, u.SourceDigest); err != nil {
			return zero, err
		}
		e = api.TeamQueueEntry{TaskID: task, Host: req.Host, State: "host_usage"}
	case "set_limit":
		if req.ConcurrencyLimit < 1 || req.ConcurrencyLimit > 2 {
			return zero, api.ErrInvalid
		}
		if req.ConcurrencyLimit > 1 {
			if req.Host == "" {
				return zero, api.ErrInvalid
			}
			if err := checkTeamHostCapacity(ctx, tx, req.Host, s.now(), 1); err != nil {
				return zero, err
			}
			var unverified int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND (state IN ('queued','launching','running') OR (state='failed' AND released_at='')) AND (repository='' OR base_commit='')`, task).Scan(&unverified); err != nil {
				return zero, err
			}
			if unverified != 0 {
				return zero, fmt.Errorf("%w: parallel queues need frozen repository and base for every outstanding item", api.ErrConflict)
			}
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND (state IN ('launching','running') OR (state='failed' AND released_at=''))`, task).Scan(&active); err != nil {
			return zero, err
		}
		if active > req.ConcurrencyLimit {
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
		if !api.ValidID(req.ItemID, "wi") || req.OrderMessageSeq < 1 || (req.Template != "" && req.Template != "planned") || req.Host == "" || req.Cwd == "" {
			return zero, api.ErrInvalid
		}
		ownership, err := canonicalQueueOwnership(req.Ownership)
		if err != nil {
			return zero, err
		}
		if strings.Contains(req.Repository, "\x00") || len(req.Repository) > 1024 {
			return zero, api.ErrInvalid
		}
		if req.Repository != "" && !validGitCommit(req.BaseCommit) {
			return zero, api.ErrInvalid
		}
		var limit int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&limit); err != nil {
			return zero, err
		}
		if limit > 1 && (req.Repository == "" || req.BaseCommit == "") {
			return zero, fmt.Errorf("%w: parallel queue entry needs a frozen repository and base", api.ErrConflict)
		}
		item, err := getWorkItem(tx, ctx, task, req.ItemID)
		if err != nil {
			return zero, err
		}
		if item.Status == "done" || item.Status == "dismissed" {
			return zero, fmt.Errorf("%w: item is terminal", api.ErrConflict)
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
		e = api.TeamQueueEntry{ID: api.NewID("tqe"), TaskID: task, ItemID: req.ItemID, ItemRevision: item.Revision, OrderMessageSeq: req.OrderMessageSeq, Template: "planned", Position: maxPos + 1, State: "queued", Revision: 1, Host: req.Host, Cwd: req.Cwd, Repository: req.Repository, Ownership: ownership, BaseCommit: req.BaseCommit}
		ownedJSON, _ := json.Marshal(ownership)
		_, err = tx.ExecContext(ctx, `INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,repository,ownership_json,base_commit,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, task, e.ItemID, e.ItemRevision, e.OrderMessageSeq, e.Template, e.Position, e.State, e.Revision, e.Host, e.Cwd, e.Repository, string(ownedJSON), e.BaseCommit, now, now)
		if err != nil {
			return zero, fmt.Errorf("%w: duplicate item or queue entry: %v", api.ErrConflict, err)
		}
	case "remove", "reorder", "claim", "freeze", "attempt", "started", "running", "replace_lead", "close", "close_refresh", "accept", "finish", "fail", "release":
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
		case "release":
			var limit int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&limit); err != nil {
				return zero, err
			}
			if e.State != "failed" || e.ReleasedAt != "" || (limit == 1 && (t.Orchestrator != "" || t.CleanupPending != 0)) {
				return zero, api.ErrConflict
			}
			if req.ReleaseProof != nil && (!req.SessionsChecked || req.Host != e.Host) {
				return zero, fmt.Errorf("%w: saved launch host proof is required", api.ErrConflict)
			}
			if err := queueReleaseSafe(ctx, tx, task, e.ItemID, e.ID, e.Host, e.LaunchJSON, req.ReleaseProof); err != nil {
				return zero, err
			}
			var reservedEntry string
			err := tx.QueryRowContext(ctx, `SELECT entry_id FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, e.ID).Scan(&reservedEntry)
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
			var limit int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&limit); err != nil {
				return zero, err
			}
			if e.State != "queued" || t.PauseState != api.ProjectPauseActive || (limit == 1 && t.Orchestrator != "") || (limit == 1 && t.CleanupPending != 0) || req.Host != e.Host || req.PauseGeneration != t.PauseGeneration {
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
			rows, err := tx.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? AND (state='queued' OR state IN ('launching','running') OR (state='failed' AND released_at='')) ORDER BY position`, task)
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
			if len(activeEntries) >= limit {
				return zero, fmt.Errorf("%w: all team slots are reserved", api.ErrConflict)
			}
			if limit > 1 {
				if err := checkTeamHostCapacity(ctx, tx, e.Host, s.now(), 1); err != nil {
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
			handlers, err := tx.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN ('retired','closed','exited') ORDER BY created_at,id`, task, api.AgentRoleDatabaseHandler)
			if err != nil {
				return zero, err
			}
			var chosen api.Agent
			for handlers.Next() {
				a, scanErr := scanAgent(handlers)
				if scanErr != nil {
					handlers.Close()
					return zero, scanErr
				}
				if !a.Online {
					continue
				}
				inUse := false
				for _, active := range activeEntries {
					if active.HandlerID == a.ID {
						inUse = true
						break
					}
				}
				if !inUse {
					chosen = a
					break
				}
			}
			err = handlers.Err()
			handlers.Close()
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
			var limit int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&limit); err != nil {
				return zero, err
			}
			if (limit > 1 || plan.HandlerID != "") && (plan.HandlerID != e.HandlerID || plan.HandlerRunID != e.HandlerRunID || plan.HandlerLeaseGeneration != e.HandlerLeaseGeneration) {
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
				var limit int
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&limit); err != nil {
					return zero, err
				}
				if limit > 1 {
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
			if e.State != "running" || len(e.CloseJSON) != 0 || !json.Valid(req.CloseJSON) || len(req.CloseJSON) == 0 {
				return zero, api.ErrConflict
			}
			e.CloseJSON = req.CloseJSON
		case "close_refresh":
			if e.State != "running" || len(e.CloseJSON) == 0 {
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
			if e.State != "running" || e.Acceptance != nil || e.Repository == "" || req.Acceptance == nil || req.HandlerAgentID != e.HandlerID || req.HandlerRunID != e.HandlerRunID {
				return zero, fmt.Errorf("%w: exact active handler and unaccepted team are required", api.ErrConflict)
			}
			var handlerRole, handlerStatus, handlerRun string
			if err := tx.QueryRowContext(ctx, `SELECT role,status,run_id FROM agents WHERE task_id=? AND id=?`, task, req.HandlerAgentID).Scan(&handlerRole, &handlerStatus, &handlerRun); err != nil || handlerRole != api.AgentRoleDatabaseHandler || handlerRun != req.HandlerRunID || handlerStatus == api.AgentClosed || handlerStatus == api.AgentExited || handlerStatus == api.AgentRetired {
				return zero, fmt.Errorf("%w: assigned handler run is unavailable", api.ErrConflict)
			}
			item, err := getWorkItem(tx, ctx, task, e.ItemID)
			if err != nil {
				return zero, err
			}
			candidate := *req.Acceptance
			if item.Status != "done" || candidate.ItemRevision != item.Revision || !reflect.DeepEqual(candidate.CompletionReport, item.CompletionReport) || candidate.Repository != e.Repository || candidate.BaseCommit != e.BaseCommit || !filepath.IsAbs(candidate.Worktree) || filepath.Clean(candidate.Worktree) != candidate.Worktree || strings.ContainsRune(candidate.Worktree, '\x00') || !validGitCommit(candidate.Commit) || candidate.Branch == "" || len(candidate.Branch) > 200 || strings.ContainsAny(candidate.Branch, "\x00\n\r") || strings.TrimSpace(candidate.Evidence) == "" || candidate.AcceptedAt != "" {
				return zero, api.ErrInvalid
			}
			candidate.AcceptedAt = now
			e.Acceptance = &candidate
		case "finish":
			if e.State != "running" || len(e.CloseJSON) == 0 {
				return zero, api.ErrConflict
			}
			var reserved string
			if err := tx.QueryRowContext(ctx, `SELECT entry_id FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, task, e.ID).Scan(&reserved); err != nil || reserved != e.ID {
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
			if item.Status == "done" && e.Repository != "" && (req.Integration == nil || e.Acceptance == nil) {
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
			_, err = tx.ExecContext(ctx, `UPDATE team_queue_entries SET state=?,revision=?,pause_generation=?,launch_json=?,close_json=?,failure=?,escalation_seq=?,released_at=?,handler_id=?,handler_run_id=?,handler_lease_generation=?,acceptance_json=?,integration_json=?,updated_at=? WHERE id=?`, e.State, e.Revision, e.PauseGeneration, string(e.LaunchJSON), string(e.CloseJSON), e.Failure, e.EscalationSeq, e.ReleasedAt, e.HandlerID, e.HandlerRunID, e.HandlerLeaseGeneration, acceptanceJSON, integrationJSON, now, e.ID)
			if err != nil {
				return zero, err
			}
		}
	default:
		return zero, api.ErrInvalid
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
