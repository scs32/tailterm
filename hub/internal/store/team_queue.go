package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

func migrateTeamQueue(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS team_queue_entries (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), item_id TEXT NOT NULL REFERENCES work_items(id),
 item_revision INTEGER NOT NULL, order_seq INTEGER NOT NULL, template TEXT NOT NULL,
 position INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('queued','launching','running','finished','failed')),
 revision INTEGER NOT NULL DEFAULT 1, host TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '',
 pause_generation INTEGER NOT NULL DEFAULT 0, launch_json BLOB NOT NULL DEFAULT '', close_json BLOB NOT NULL DEFAULT '',
 failure TEXT NOT NULL DEFAULT '', escalation_seq INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(task_id,item_id));
 CREATE INDEX IF NOT EXISTS team_queue_order ON team_queue_entries(task_id,position);
 CREATE UNIQUE INDEX IF NOT EXISTS team_queue_active ON team_queue_entries(task_id) WHERE state IN ('launching','running');
 CREATE TABLE IF NOT EXISTS team_queue_requests(task_id TEXT NOT NULL,request_id TEXT NOT NULL,payload_hash TEXT NOT NULL,result_json BLOB NOT NULL,PRIMARY KEY(task_id,request_id));
 CREATE TABLE IF NOT EXISTS team_launch_reservations(task_id TEXT PRIMARY KEY REFERENCES tasks(id),entry_id TEXT NOT NULL DEFAULT '',item_id TEXT NOT NULL, token TEXT NOT NULL,
 pause_generation INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('reserved','launching','running')), created_at TEXT NOT NULL);`)
	return err
}

func validTeamQueueID(id string) bool {
	if !strings.HasPrefix(id, "tqe_") || len(id) != 20 {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
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

const teamQueueCols = `id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,close_json,failure,escalation_seq`

func scanTeamQueue(row interface{ Scan(...any) error }) (api.TeamQueueEntry, error) {
	var e api.TeamQueueEntry
	var launch, close []byte
	err := row.Scan(&e.ID, &e.TaskID, &e.ItemID, &e.ItemRevision, &e.OrderMessageSeq, &e.Template, &e.Position, &e.State, &e.Revision, &e.Host, &e.Cwd, &e.PauseGeneration, &launch, &close, &e.Failure, &e.EscalationSeq)
	if err != nil {
		return e, err
	}
	if len(launch) > 0 {
		e.LaunchJSON = append([]byte(nil), launch...)
	}
	if len(close) > 0 {
		e.CloseJSON = append([]byte(nil), close...)
	}
	return e, nil
}

func (s *Store) ListTeamQueue(ctx context.Context, task string) (api.TeamQueueList, error) {
	if !api.ValidID(task, "tsk") {
		return api.TeamQueueList{}, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+teamQueueCols+` FROM team_queue_entries WHERE task_id=? ORDER BY position`, task)
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
	return out, rows.Err()
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
	return out, rows.Err()
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
	b, _ := json.Marshal(req)
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
	if req.Operation == "claim" || req.Operation == "manual" {
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
	case "manual":
		if !api.ValidID(req.ItemID, "wi") || req.OrderMessageSeq < 1 || req.PauseGeneration != t.PauseGeneration || t.Orchestrator != "" || t.CleanupPending != 0 || t.PauseState != api.ProjectPauseActive {
			return zero, api.ErrConflict
		}
		var queued int
		_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND item_id=?`, task, req.ItemID).Scan(&queued)
		if queued > 0 {
			return zero, fmt.Errorf("%w: item is already in team queue", api.ErrConflict)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO team_launch_reservations(task_id,entry_id,item_id,token,pause_generation,state,created_at) VALUES(?,?,?,?,?,?,?)`, task, "", req.ItemID, req.RequestID, t.PauseGeneration, "reserved", now)
		if err != nil {
			return zero, fmt.Errorf("%w: another team launch is reserved", api.ErrConflict)
		}
		e = api.TeamQueueEntry{TaskID: task, ItemID: req.ItemID, OrderMessageSeq: req.OrderMessageSeq, State: "launching", PauseGeneration: t.PauseGeneration}
	case "add":
		if !api.ValidID(req.ItemID, "wi") || req.OrderMessageSeq < 1 || (req.Template != "" && req.Template != "planned") || req.Host == "" || req.Cwd == "" {
			return zero, api.ErrInvalid
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
		var maxPos int64
		_ = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position),0) FROM team_queue_entries WHERE task_id=?`, task).Scan(&maxPos)
		e = api.TeamQueueEntry{ID: api.NewID("tqe"), TaskID: task, ItemID: req.ItemID, ItemRevision: item.Revision, OrderMessageSeq: req.OrderMessageSeq, Template: "planned", Position: maxPos + 1, State: "queued", Revision: 1, Host: req.Host, Cwd: req.Cwd}
		_, err = tx.ExecContext(ctx, `INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, task, e.ItemID, e.ItemRevision, e.OrderMessageSeq, e.Template, e.Position, e.State, e.Revision, e.Host, e.Cwd, now, now)
		if err != nil {
			return zero, fmt.Errorf("%w: duplicate item or queue entry: %v", api.ErrConflict, err)
		}
	case "remove", "reorder", "claim", "freeze", "attempt", "started", "running", "close", "finish", "fail":
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
			if e.State != "queued" || t.PauseState != api.ProjectPauseActive || t.Orchestrator != "" || t.CleanupPending != 0 || req.Host != e.Host || req.PauseGeneration != t.PauseGeneration {
				return zero, fmt.Errorf("%w: project is not launchable", api.ErrConflict)
			}
			var head string
			err = tx.QueryRowContext(ctx, `SELECT id FROM team_queue_entries WHERE task_id=? AND state='queued' ORDER BY position LIMIT 1`, task).Scan(&head)
			if err != nil || head != e.ID {
				return zero, fmt.Errorf("%w: not queue head", api.ErrConflict)
			}
			var active int
			_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM team_launch_reservations WHERE task_id=?`, task).Scan(&active)
			if active > 0 {
				return zero, fmt.Errorf("%w: launch is reserved", api.ErrConflict)
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO team_launch_reservations(task_id,entry_id,item_id,token,pause_generation,state,created_at) VALUES(?,?,?,?,?,?,?)`, task, e.ID, e.ItemID, req.RequestID, t.PauseGeneration, "reserved", now); err != nil {
				return zero, err
			}
			e.State = "launching"
			e.PauseGeneration = t.PauseGeneration
		case "freeze":
			if e.State != "launching" || len(e.LaunchJSON) != 0 || !json.Valid(req.LaunchJSON) || len(req.LaunchJSON) == 0 {
				return zero, api.ErrConflict
			}
			var plan struct {
				Task     string          `json:"task"`
				Item     string          `json:"item"`
				Revision int64           `json:"revision"`
				Order    int64           `json:"order"`
				Context  json.RawMessage `json:"context"`
				Members  []struct {
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
			seenAgents := map[string]bool{}
			seenRuns := map[string]bool{}
			for _, member := range plan.Members {
				if !api.ValidID(member.Fields.AgentID, "agt") || !validRunID(member.RunID) || member.Fields.Name == "" || member.Fields.Cwd == "" || member.State != "unstarted" || seenAgents[member.Fields.AgentID] || seenRuns[member.RunID] {
					return zero, api.ErrInvalid
				}
				seenAgents[member.Fields.AgentID] = true
				seenRuns[member.RunID] = true
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
			if err := tx.QueryRowContext(ctx, `SELECT entry_id,pause_generation FROM team_launch_reservations WHERE task_id=?`, task).Scan(&reserved, &generation); err != nil {
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
		case "close":
			if e.State != "running" || len(e.CloseJSON) != 0 || !json.Valid(req.CloseJSON) || len(req.CloseJSON) == 0 {
				return zero, api.ErrConflict
			}
			e.CloseJSON = req.CloseJSON
		case "finish":
			if e.State != "running" || len(e.CloseJSON) == 0 {
				return zero, api.ErrConflict
			}
			var reserved string
			if err := tx.QueryRowContext(ctx, `SELECT entry_id FROM team_launch_reservations WHERE task_id=?`, task).Scan(&reserved); err != nil || reserved != e.ID {
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
			if json.Unmarshal([]byte(receiptJSON), &result) != nil || result.TaskID != task || result.ItemID != e.ItemID || len(result.Members) == 0 || t.Orchestrator != "" {
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
			_, err = tx.ExecContext(ctx, `UPDATE team_queue_entries SET state=?,revision=?,pause_generation=?,launch_json=?,close_json=?,failure=?,escalation_seq=?,updated_at=? WHERE id=?`, e.State, e.Revision, e.PauseGeneration, string(e.LaunchJSON), string(e.CloseJSON), e.Failure, e.EscalationSeq, now, e.ID)
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
