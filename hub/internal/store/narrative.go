package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const narrativeSchema = `
CREATE TABLE IF NOT EXISTS narrative_state (
  task_id TEXT NOT NULL, item_id TEXT NOT NULL, current_seq INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(task_id,item_id), FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE TABLE IF NOT EXISTS narrative_artifacts (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL, item_id TEXT NOT NULL,
  namespace TEXT NOT NULL, source_id TEXT NOT NULL, latest_version INTEGER NOT NULL,
  created_at TEXT NOT NULL, UNIQUE(task_id,item_id,namespace,source_id),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE INDEX IF NOT EXISTS narrative_artifacts_item ON narrative_artifacts(task_id,item_id,id);
CREATE TABLE IF NOT EXISTS narrative_artifact_versions (
  artifact_id TEXT NOT NULL REFERENCES narrative_artifacts(id), version INTEGER NOT NULL,
  task_id TEXT NOT NULL, item_id TEXT NOT NULL, narrative_seq INTEGER NOT NULL,
  source_version TEXT NOT NULL, kind TEXT NOT NULL, title TEXT NOT NULL,
  original_author TEXT NOT NULL, source_time TEXT, ingested_by TEXT NOT NULL,
  ingested_at TEXT NOT NULL, provenance TEXT NOT NULL, capture_state TEXT NOT NULL,
  availability TEXT NOT NULL, content TEXT NOT NULL DEFAULT '', content_digest TEXT NOT NULL DEFAULT '',
  supplied_digest TEXT NOT NULL DEFAULT '', locator TEXT NOT NULL DEFAULT '', size INTEGER NOT NULL DEFAULT 0,
  supersedes_version INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(artifact_id,version), UNIQUE(task_id,item_id,narrative_seq),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE INDEX IF NOT EXISTS narrative_artifact_versions_item ON narrative_artifact_versions(task_id,item_id,narrative_seq);
CREATE TABLE IF NOT EXISTS narrative_links (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL, item_id TEXT NOT NULL, latest_revision INTEGER NOT NULL,
  created_at TEXT NOT NULL, FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE TABLE IF NOT EXISTS narrative_link_versions (
  link_id TEXT NOT NULL REFERENCES narrative_links(id), revision INTEGER NOT NULL,
  task_id TEXT NOT NULL, item_id TEXT NOT NULL, narrative_seq INTEGER NOT NULL,
  action TEXT NOT NULL, relationship TEXT NOT NULL, target TEXT NOT NULL,
  feature_revision INTEGER NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '', source_time TEXT,
  created_by TEXT NOT NULL, created_at TEXT NOT NULL,
  PRIMARY KEY(link_id,revision), UNIQUE(task_id,item_id,narrative_seq),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE INDEX IF NOT EXISTS narrative_link_versions_item ON narrative_link_versions(task_id,item_id,narrative_seq);
CREATE TABLE IF NOT EXISTS narrative_coverage (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL, item_id TEXT NOT NULL, source TEXT NOT NULL,
  latest_revision INTEGER NOT NULL, created_at TEXT NOT NULL,
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE TABLE IF NOT EXISTS narrative_coverage_versions (
  coverage_id TEXT NOT NULL REFERENCES narrative_coverage(id), revision INTEGER NOT NULL,
  task_id TEXT NOT NULL, item_id TEXT NOT NULL, narrative_seq INTEGER NOT NULL,
  source TEXT NOT NULL, scope TEXT NOT NULL, capture_state TEXT NOT NULL,
  captured_ids TEXT NOT NULL, known_gaps TEXT NOT NULL, unknown_extent INTEGER NOT NULL,
  as_of TEXT, assessment TEXT NOT NULL, assessment_text TEXT NOT NULL DEFAULT '',
  evidence_refs TEXT NOT NULL DEFAULT '[]', assessment_by TEXT NOT NULL DEFAULT '{}',
  created_by TEXT NOT NULL, created_at TEXT NOT NULL,
  PRIMARY KEY(coverage_id,revision), UNIQUE(task_id,item_id,narrative_seq),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE INDEX IF NOT EXISTS narrative_coverage_versions_item ON narrative_coverage_versions(task_id,item_id,narrative_seq);
CREATE TABLE IF NOT EXISTS narrative_reports (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL, item_id TEXT NOT NULL, latest_version INTEGER NOT NULL,
  created_at TEXT NOT NULL, FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE TABLE IF NOT EXISTS narrative_report_versions (
  report_id TEXT NOT NULL REFERENCES narrative_reports(id), version INTEGER NOT NULL,
  task_id TEXT NOT NULL, item_id TEXT NOT NULL, narrative_seq INTEGER NOT NULL,
  scope_revision INTEGER NOT NULL, sections TEXT NOT NULL, refs TEXT NOT NULL, digest TEXT NOT NULL,
  created_by TEXT NOT NULL, created_at TEXT NOT NULL,
  PRIMARY KEY(report_id,version), UNIQUE(task_id,item_id,narrative_seq),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE INDEX IF NOT EXISTS narrative_report_versions_item ON narrative_report_versions(task_id,item_id,narrative_seq);
CREATE TABLE IF NOT EXISTS narrative_timeline (
  task_id TEXT NOT NULL, item_id TEXT NOT NULL, seq INTEGER NOT NULL,
  kind TEXT NOT NULL, object_id TEXT NOT NULL, object_version INTEGER NOT NULL,
  source TEXT NOT NULL DEFAULT '', relationship TEXT NOT NULL DEFAULT '', capture_state TEXT NOT NULL DEFAULT '',
  source_time TEXT, created_at TEXT NOT NULL, PRIMARY KEY(task_id,item_id,seq),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE TABLE IF NOT EXISTS narrative_receipts (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL, item_id TEXT NOT NULL,
  operation TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', by_node TEXT NOT NULL, by_user TEXT NOT NULL,
  request_id TEXT NOT NULL, payload_hash TEXT NOT NULL, result TEXT NOT NULL, created_at TEXT NOT NULL,
  UNIQUE(task_id,item_id,operation,agent_id,by_node,by_user,request_id),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE TABLE IF NOT EXISTS narrative_completion_pins (
  task_id TEXT NOT NULL, item_id TEXT NOT NULL, item_revision INTEGER NOT NULL,
  report_id TEXT NOT NULL, report_version INTEGER NOT NULL, report_digest TEXT NOT NULL,
  scope_revision INTEGER NOT NULL, actor TEXT NOT NULL, created_at TEXT NOT NULL,
  PRIMARY KEY(task_id,item_id,item_revision),
  FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id),
  FOREIGN KEY(report_id,report_version) REFERENCES narrative_report_versions(report_id,version)
);`

func migrateNarrative(db *sql.DB) error {
	if _, err := db.Exec(narrativeSchema); err != nil {
		return err
	}
	for _, c := range []struct{ name, definition string }{
		{"evidence_refs", "TEXT NOT NULL DEFAULT '[]'"},
		{"assessment_by", "TEXT NOT NULL DEFAULT '{}'"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('narrative_coverage_versions') WHERE name=?`, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE narrative_coverage_versions ADD COLUMN " + c.name + " " + c.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

type narrativeCursor struct{ High, After int64 }

func encodeNarrativeCursor(c narrativeCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeNarrativeCursor(raw string) (narrativeCursor, error) {
	if raw == "" {
		return narrativeCursor{}, nil
	}
	var c narrativeCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || c.High < 0 || c.After < 0 || c.After > c.High {
		return narrativeCursor{}, api.ErrInvalid
	}
	return c, nil
}

func validNarrativeID(id, prefix string) bool {
	if len(id) != len(prefix)+17 || !strings.HasPrefix(id, prefix+"_") {
		return false
	}
	_, err := hex.DecodeString(id[len(prefix)+1:])
	return err == nil
}

func validNarrativeText(s string, max int, required bool) bool {
	return (!required || strings.TrimSpace(s) != "") && api.ValidText(s, max)
}

func validCaptureState(s string) bool {
	switch s {
	case "stored-content", "reference-only", "not-ingested", "unavailable":
		return true
	}
	return false
}

func validAvailability(s string) bool {
	switch s {
	case "unknown", "available", "unavailable", "last-observed":
		return true
	}
	return false
}

func validAssessment(s string) bool {
	switch s {
	case "unverified", "passed", "failed", "independently-verified":
		return true
	}
	return false
}

func validNarrativeSender(sender api.Sender, required bool) bool {
	if required && sender.AgentID == "" && strings.TrimSpace(sender.Node) == "" && strings.TrimSpace(sender.User) == "" {
		return false
	}
	return (sender.AgentID == "" || api.ValidID(sender.AgentID, "agt")) && validNarrativeText(sender.Node, 512, false) && validNarrativeText(sender.User, 512, false)
}

func validLocator(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && len(raw) <= 2048
}

func contentDigest(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func narrativeActor(agentID, runID string, by api.Caller) api.NarrativeActor {
	return api.NarrativeActor{AgentID: agentID, RunID: runID, Caller: by}
}

func validateNarrativeActor(q queryRower, ctx context.Context, taskID, agentID, runID string) error {
	if err := validateWorkItemAgent(q, ctx, taskID, agentID); err != nil {
		return err
	}
	if runID == "" {
		return nil
	}
	if agentID == "" || !validRunID(runID) {
		return api.ErrInvalid
	}
	var current string
	if err := q.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE task_id=? AND id=?`, taskID, agentID).Scan(&current); err != nil {
		return err
	}
	if current != runID {
		return workItemConflict("agent run changed; refresh identity before writing narrative history")
	}
	return nil
}

func loadWritableFeature(q queryRower, ctx context.Context, taskID, itemID string) (api.WorkItem, error) {
	task, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.WorkItem{}, api.ErrNotFound
	}
	if err != nil {
		return api.WorkItem{}, err
	}
	if task.Status != api.TaskOpen {
		return api.WorkItem{}, api.ErrClosed
	}
	item, err := getWorkItem(q, ctx, taskID, itemID)
	if err != nil {
		return item, err
	}
	if item.Kind != "feature" {
		return item, api.ErrInvalid
	}
	return item, nil
}

func nextNarrativeSeq(ctx context.Context, tx *sql.Tx, taskID, itemID string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO narrative_state(task_id,item_id,current_seq) VALUES(?,?,1)
ON CONFLICT(task_id,item_id) DO UPDATE SET current_seq=current_seq+1 RETURNING current_seq`, taskID, itemID).Scan(&seq)
	return seq, err
}

func currentNarrativeSeq(q queryRower, ctx context.Context, taskID, itemID string) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, `SELECT current_seq FROM narrative_state WHERE task_id=? AND item_id=?`, taskID, itemID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

func narrativePayloadHash(v any) string { return requestHash(v) }

func findNarrativeReceipt(q queryRower, ctx context.Context, taskID, itemID, operation, requestID, agentID string, by api.Caller) (api.NarrativeReceipt, string, error) {
	var r api.NarrativeReceipt
	var created, payload, result string
	err := q.QueryRowContext(ctx, `SELECT id,request_id,operation,task_id,item_id,result,created_at,payload_hash FROM narrative_receipts WHERE task_id=? AND item_id=? AND operation=? AND agent_id=? AND by_node=? AND by_user=? AND request_id=?`, taskID, itemID, operation, agentID, by.Node, by.User, requestID).Scan(&r.ID, &r.RequestID, &r.Operation, &r.TaskID, &r.ItemID, &result, &created, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return r, "", api.ErrNotFound
	}
	if err == nil {
		r.Result = json.RawMessage(result)
		r.CreatedAt = parseTS(created)
	}
	return r, payload, err
}

func saveNarrativeReceipt(ctx context.Context, tx *sql.Tx, taskID, itemID, operation, requestID, agentID, payload string, result any, by api.Caller, now time.Time) (api.NarrativeReceipt, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return api.NarrativeReceipt{}, err
	}
	r := api.NarrativeReceipt{ID: api.NewID("nrr"), RequestID: requestID, Operation: operation, TaskID: taskID, ItemID: itemID, Result: raw, CreatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_receipts(id,task_id,item_id,operation,agent_id,by_node,by_user,request_id,payload_hash,result,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, r.ID, taskID, itemID, operation, agentID, by.Node, by.User, requestID, payload, string(raw), ts(now))
	return r, err
}

func replayNarrativeReceipt[T any](q queryRower, ctx context.Context, taskID, itemID, operation, requestID, agentID, payload string, by api.Caller) (T, bool, error) {
	var zero T
	r, prior, err := findNarrativeReceipt(q, ctx, taskID, itemID, operation, requestID, agentID, by)
	if errors.Is(err, api.ErrNotFound) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, err
	}
	if prior != payload {
		return zero, false, workItemConflict("request ID was already used with different narrative data")
	}
	if err := json.Unmarshal(r.Result, &zero); err != nil {
		return zero, false, err
	}
	return zero, true, nil
}

func marshalJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func appendNarrativeMetadata[T any](values []T, value T) ([]T, bool) {
	next := append(values, value)
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > api.MaxNarrativeResponseBytes-(64<<10) {
		return values, false
	}
	return next, true
}

func parseOptionalTime(raw sql.NullString) *time.Time {
	if !raw.Valid {
		return nil
	}
	t := parseTS(raw.String)
	return &t
}

func scanArtifactVersion(row rowScanner) (api.NarrativeArtifactVersion, error) {
	var v api.NarrativeArtifactVersion
	var authorRaw, actorRaw, sourceRaw, ingested string
	err := row.Scan(&v.ArtifactID, &v.TaskID, &v.ItemID, &v.Version, &v.NarrativeSeq, &v.Namespace, &v.SourceID, &v.SourceVersion, &v.Kind, &v.Title, &authorRaw, &sourceRaw, &actorRaw, &ingested, &v.Provenance, &v.CaptureState, &v.Availability, &v.Content, &v.ContentDigest, &v.SuppliedDigest, &v.Locator, &v.Size, &v.SupersedesVersion)
	if err != nil {
		return v, err
	}
	_ = json.Unmarshal([]byte(authorRaw), &v.OriginalAuthor)
	_ = json.Unmarshal([]byte(actorRaw), &v.IngestedBy)
	v.IngestedAt = parseTS(ingested)
	if sourceRaw != "" {
		t := parseTS(sourceRaw)
		v.SourceTime = &t
	}
	return v, nil
}

const artifactVersionCols = `v.artifact_id,v.task_id,v.item_id,v.version,v.narrative_seq,a.namespace,a.source_id,v.source_version,v.kind,v.title,v.original_author,COALESCE(v.source_time,''),v.ingested_by,v.ingested_at,v.provenance,v.capture_state,v.availability,v.content,v.content_digest,v.supplied_digest,v.locator,v.size,v.supersedes_version`

func validateArtifactRequest(req api.PutNarrativeArtifactRequest) error {
	if !validRequestID(req.RequestID) || req.ExpectedVersion < 0 || !validNarrativeText(req.Namespace, 128, true) || !validNarrativeText(req.SourceID, 512, true) || !validNarrativeText(req.SourceVersion, 256, true) || !validNarrativeText(req.Kind, 64, true) || !validNarrativeText(req.Title, 512, true) || !validNarrativeText(req.Provenance, 128, true) || !validCaptureState(req.CaptureState) || !validAvailability(req.Availability) || !validNarrativeText(req.SuppliedDigest, 256, false) || !validLocator(req.Locator) || req.Size < 0 || (req.OriginalAuthor.AgentID != "" && !api.ValidID(req.OriginalAuthor.AgentID, "agt")) || !validNarrativeText(req.OriginalAuthor.Node, 512, false) || !validNarrativeText(req.OriginalAuthor.User, 512, false) {
		return api.ErrInvalid
	}
	if req.ArtifactID != "" && !validNarrativeID(req.ArtifactID, "nart") {
		return api.ErrInvalid
	}
	if !api.ValidText(req.Content, api.MaxNarrativeContentBytes) {
		return api.ErrInvalid
	}
	switch req.CaptureState {
	case "stored-content":
		if req.Content == "" || req.Locator != "" {
			return api.ErrInvalid
		}
	case "reference-only":
		if req.Content != "" || req.Locator == "" {
			return api.ErrInvalid
		}
	default:
		if req.Content != "" {
			return api.ErrInvalid
		}
	}
	return nil
}

func (s *Store) PutNarrativeArtifact(ctx context.Context, taskID, itemID string, req api.PutNarrativeArtifactRequest, by api.Caller) (api.NarrativeArtifactVersion, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || validateArtifactRequest(req) != nil {
		return api.NarrativeArtifactVersion{}, false, api.ErrInvalid
	}
	payload := narrativePayloadHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	defer tx.Rollback()
	if prior, ok, err := replayNarrativeReceipt[api.NarrativeArtifactVersion](tx, ctx, taskID, itemID, "artifact", req.RequestID, req.AgentID, payload, by); ok || err != nil {
		return prior, ok, err
	}
	if _, err = loadWritableFeature(tx, ctx, taskID, itemID); err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	if err = validateNarrativeActor(tx, ctx, taskID, req.AgentID, req.RunID); err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	id, current := req.ArtifactID, int64(0)
	if id == "" {
		id = api.NewID("nart")
		if req.ExpectedVersion != 0 {
			return api.NarrativeArtifactVersion{}, false, api.ErrInvalid
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT id FROM narrative_artifacts WHERE task_id=? AND item_id=? AND namespace=? AND source_id=?`, taskID, itemID, req.Namespace, req.SourceID).Scan(&existing)
		if err == nil {
			return api.NarrativeArtifactVersion{}, false, workItemConflict("artifact source identity already exists; supply its ID and expected version")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return api.NarrativeArtifactVersion{}, false, err
		}
	} else {
		err = tx.QueryRowContext(ctx, `SELECT latest_version FROM narrative_artifacts WHERE id=? AND task_id=? AND item_id=? AND namespace=? AND source_id=?`, id, taskID, itemID, req.Namespace, req.SourceID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return api.NarrativeArtifactVersion{}, false, api.ErrNotFound
		}
		if err != nil {
			return api.NarrativeArtifactVersion{}, false, err
		}
		if current != req.ExpectedVersion {
			return api.NarrativeArtifactVersion{}, false, workItemConflict("artifact version changed; refresh before correcting it")
		}
	}
	now := s.now()
	version := current + 1
	seq, err := nextNarrativeSeq(ctx, tx, taskID, itemID)
	if err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	v := api.NarrativeArtifactVersion{ArtifactID: id, TaskID: taskID, ItemID: itemID, Version: version, NarrativeSeq: seq, Namespace: req.Namespace, SourceID: req.SourceID, SourceVersion: req.SourceVersion, Kind: req.Kind, Title: req.Title, OriginalAuthor: req.OriginalAuthor, SourceTime: req.SourceTime, IngestedBy: narrativeActor(req.AgentID, req.RunID, by), IngestedAt: now, Provenance: req.Provenance, CaptureState: req.CaptureState, Availability: req.Availability, Content: req.Content, SuppliedDigest: req.SuppliedDigest, Locator: req.Locator, Size: req.Size, SupersedesVersion: current}
	if req.CaptureState == "stored-content" {
		v.ContentDigest = contentDigest(req.Content)
		v.Size = int64(len(req.Content))
	}
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO narrative_artifacts(id,task_id,item_id,namespace,source_id,latest_version,created_at) VALUES(?,?,?,?,?,?,?)`, id, taskID, itemID, req.Namespace, req.SourceID, version, ts(now))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE narrative_artifacts SET latest_version=? WHERE id=? AND latest_version=?`, version, id, current)
	}
	if err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	var sourceTime any
	if req.SourceTime != nil {
		sourceTime = ts(*req.SourceTime)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_artifact_versions(artifact_id,version,task_id,item_id,narrative_seq,source_version,kind,title,original_author,source_time,ingested_by,ingested_at,provenance,capture_state,availability,content,content_digest,supplied_digest,locator,size,supersedes_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, version, taskID, itemID, seq, req.SourceVersion, req.Kind, req.Title, marshalJSON(req.OriginalAuthor), sourceTime, marshalJSON(v.IngestedBy), ts(now), req.Provenance, req.CaptureState, req.Availability, req.Content, v.ContentDigest, req.SuppliedDigest, req.Locator, v.Size, current)
	if err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_timeline(task_id,item_id,seq,kind,object_id,object_version,source,capture_state,source_time,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, taskID, itemID, seq, "artifact", id, version, req.Namespace, req.CaptureState, sourceTime, ts(now))
	if err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	if _, err = saveNarrativeReceipt(ctx, tx, taskID, itemID, "artifact", req.RequestID, req.AgentID, payload, v, by, now); err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return api.NarrativeArtifactVersion{}, false, err
	}
	s.notify(taskID)
	return v, false, nil
}

func (s *Store) GetNarrativeArtifactVersion(ctx context.Context, taskID, itemID, artifactID string, version int64) (api.NarrativeArtifactVersion, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !validNarrativeID(artifactID, "nart") || version < 1 {
		return api.NarrativeArtifactVersion{}, api.ErrInvalid
	}
	if _, err := s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeArtifactVersion{}, err
	}
	v, err := scanArtifactVersion(s.db.QueryRowContext(ctx, `SELECT `+artifactVersionCols+` FROM narrative_artifact_versions v JOIN narrative_artifacts a ON a.id=v.artifact_id WHERE v.task_id=? AND v.item_id=? AND v.artifact_id=? AND v.version=?`, taskID, itemID, artifactID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return v, api.ErrNotFound
	}
	return v, err
}

func (s *Store) ListNarrativeArtifacts(ctx context.Context, taskID, itemID, cursor string, limit int) (api.NarrativeArtifactList, error) {
	if limit < 1 || limit > api.MaxNarrativePage {
		return api.NarrativeArtifactList{}, api.ErrInvalid
	}
	c, err := decodeNarrativeCursor(cursor)
	if err != nil {
		return api.NarrativeArtifactList{}, err
	}
	if _, err = s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeArtifactList{}, err
	}
	if c.High == 0 {
		c.High, err = currentNarrativeSeq(s.db, ctx, taskID, itemID)
		if err != nil {
			return api.NarrativeArtifactList{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+artifactVersionCols+` FROM narrative_artifact_versions v JOIN narrative_artifacts a ON a.id=v.artifact_id WHERE v.task_id=? AND v.item_id=? AND v.narrative_seq>? AND v.narrative_seq<=? AND v.version=(SELECT max(v2.version) FROM narrative_artifact_versions v2 WHERE v2.artifact_id=v.artifact_id AND v2.narrative_seq<=?) ORDER BY v.narrative_seq LIMIT ?`, taskID, itemID, c.After, c.High, c.High, limit+1)
	if err != nil {
		return api.NarrativeArtifactList{}, err
	}
	defer rows.Close()
	out := api.NarrativeArtifactList{Artifacts: []api.NarrativeArtifactSummary{}}
	var last int64
	hasMore := false
	for rows.Next() {
		v, e := scanArtifactVersion(rows)
		if e != nil {
			return out, e
		}
		v.Content = ""
		if len(out.Artifacts) == limit {
			hasMore = true
			break
		}
		next, fits := appendNarrativeMetadata(out.Artifacts, api.NarrativeArtifactSummary{ArtifactID: v.ArtifactID, Namespace: v.Namespace, SourceID: v.SourceID, Latest: v})
		if !fits {
			hasMore = true
			break
		}
		out.Artifacts = next
		last = v.NarrativeSeq
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if hasMore {
		out.NextCursor = encodeNarrativeCursor(narrativeCursor{c.High, last})
	}
	return out, nil
}

func (s *Store) ListNarrativeArtifactVersions(ctx context.Context, taskID, itemID, artifactID, cursor string, limit int) (api.NarrativeArtifactVersionList, error) {
	if limit < 1 || limit > api.MaxNarrativePage || !validNarrativeID(artifactID, "nart") {
		return api.NarrativeArtifactVersionList{}, api.ErrInvalid
	}
	c, err := decodeNarrativeCursor(cursor)
	if err != nil {
		return api.NarrativeArtifactVersionList{}, err
	}
	if _, err = s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeArtifactVersionList{}, err
	}
	if c.High == 0 {
		c.High, err = currentNarrativeSeq(s.db, ctx, taskID, itemID)
		if err != nil {
			return api.NarrativeArtifactVersionList{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+artifactVersionCols+` FROM narrative_artifact_versions v JOIN narrative_artifacts a ON a.id=v.artifact_id WHERE v.task_id=? AND v.item_id=? AND v.artifact_id=? AND v.narrative_seq>? AND v.narrative_seq<=? ORDER BY v.narrative_seq LIMIT ?`, taskID, itemID, artifactID, c.After, c.High, limit+1)
	if err != nil {
		return api.NarrativeArtifactVersionList{}, err
	}
	defer rows.Close()
	out := api.NarrativeArtifactVersionList{Versions: []api.NarrativeArtifactVersion{}}
	var last int64
	hasMore := false
	for rows.Next() {
		v, e := scanArtifactVersion(rows)
		if e != nil {
			return out, e
		}
		v.Content = ""
		if len(out.Versions) == limit {
			hasMore = true
			break
		}
		next, fits := appendNarrativeMetadata(out.Versions, v)
		if !fits {
			hasMore = true
			break
		}
		out.Versions = next
		last = v.NarrativeSeq
	}
	if hasMore {
		out.NextCursor = encodeNarrativeCursor(narrativeCursor{c.High, last})
	}
	return out, rows.Err()
}

func validateNarrativeReference(q queryRower, ctx context.Context, taskID, itemID string, r api.NarrativeReference) error {
	if !validNarrativeText(r.Label, 512, false) || !validNarrativeText(r.Digest, 256, false) || !validLocator(r.Locator) {
		return api.ErrInvalid
	}
	switch r.Kind {
	case "message":
		var n int
		if r.TaskID != taskID || r.MessageSeq < 1 {
			return api.ErrInvalid
		}
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE task_id=? AND seq=?`, taskID, r.MessageSeq).Scan(&n); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return api.ErrInvalid
		}
	case "decision-request":
		var n int
		if r.TaskID != taskID || r.MessageSeq < 1 {
			return api.ErrInvalid
		}
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM decision_requests WHERE task_id=? AND message_seq=?`, taskID, r.MessageSeq).Scan(&n); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return api.ErrInvalid
		}
	case "decision-answer":
		var n int
		if r.TaskID != taskID || r.MessageSeq < 1 {
			return api.ErrInvalid
		}
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM decision_answers WHERE task_id=? AND message_seq=?`, taskID, r.MessageSeq).Scan(&n); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return api.ErrInvalid
		}
	case "work-item-revision":
		if r.TaskID != taskID || r.ItemID == "" || r.Revision < 1 {
			return api.ErrInvalid
		}
		var n int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, taskID, r.ItemID, r.Revision).Scan(&n); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return api.ErrInvalid
		}
	case "artifact-version":
		if !validNarrativeID(r.ArtifactID, "nart") || r.Version < 1 {
			return api.ErrInvalid
		}
		var n int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM narrative_artifact_versions WHERE task_id=? AND item_id=? AND artifact_id=? AND version=?`, taskID, itemID, r.ArtifactID, r.Version).Scan(&n); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return api.ErrInvalid
		}
	case "external":
		if !validNarrativeText(r.SourceID, 512, true) || !validLocator(r.Locator) || r.Locator == "" {
			return api.ErrInvalid
		}
	case "aiv":
		if !validNarrativeText(r.SourceID, 512, true) || r.Version < 1 || r.Digest == "" {
			return api.ErrInvalid
		}
	default:
		return api.ErrInvalid
	}
	return nil
}

func scanLinkVersion(row rowScanner) (api.NarrativeLinkVersion, error) {
	var v api.NarrativeLinkVersion
	var target, actor, source, created string
	err := row.Scan(&v.LinkID, &v.TaskID, &v.ItemID, &v.Revision, &v.NarrativeSeq, &v.Action, &v.Relationship, &target, &v.FeatureRevision, &v.Reason, &source, &actor, &created)
	if err != nil {
		return v, err
	}
	_ = json.Unmarshal([]byte(target), &v.Target)
	_ = json.Unmarshal([]byte(actor), &v.CreatedBy)
	v.CreatedAt = parseTS(created)
	if source != "" {
		t := parseTS(source)
		v.SourceTime = &t
	}
	return v, nil
}

const linkVersionCols = `link_id,task_id,item_id,revision,narrative_seq,action,relationship,target,feature_revision,reason,COALESCE(source_time,''),created_by,created_at`

func (s *Store) PutNarrativeLink(ctx context.Context, taskID, itemID string, req api.PutNarrativeLinkRequest, by api.Caller) (api.NarrativeLinkVersion, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(req.RequestID) || req.ExpectedRevision < 0 || (req.LinkID != "" && !validNarrativeID(req.LinkID, "nlnk")) || !validNarrativeText(req.Relationship, 128, true) || !validNarrativeText(req.Reason, 1024, false) || (req.Action != "link" && req.Action != "retract") || (req.Action == "retract" && strings.TrimSpace(req.Reason) == "") {
		return api.NarrativeLinkVersion{}, false, api.ErrInvalid
	}
	payload := narrativePayloadHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.NarrativeLinkVersion{}, false, err
	}
	defer tx.Rollback()
	if prior, ok, e := replayNarrativeReceipt[api.NarrativeLinkVersion](tx, ctx, taskID, itemID, "link", req.RequestID, req.AgentID, payload, by); ok || e != nil {
		return prior, ok, e
	}
	item, err := loadWritableFeature(tx, ctx, taskID, itemID)
	if err != nil {
		return api.NarrativeLinkVersion{}, false, err
	}
	if err = validateNarrativeActor(tx, ctx, taskID, req.AgentID, req.RunID); err != nil {
		return api.NarrativeLinkVersion{}, false, err
	}
	if err = validateNarrativeReference(tx, ctx, taskID, itemID, req.Target); err != nil {
		return api.NarrativeLinkVersion{}, false, err
	}
	if req.FeatureRevision > item.Revision {
		return api.NarrativeLinkVersion{}, false, api.ErrInvalid
	}
	id, current := req.LinkID, int64(0)
	if id == "" {
		if req.ExpectedRevision != 0 || req.Action != "link" {
			return api.NarrativeLinkVersion{}, false, api.ErrInvalid
		}
		id = api.NewID("nlnk")
	} else {
		err = tx.QueryRowContext(ctx, `SELECT latest_revision FROM narrative_links WHERE id=? AND task_id=? AND item_id=?`, id, taskID, itemID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return api.NarrativeLinkVersion{}, false, api.ErrNotFound
		}
		if err != nil {
			return api.NarrativeLinkVersion{}, false, err
		}
		if current != req.ExpectedRevision {
			return api.NarrativeLinkVersion{}, false, workItemConflict("link revision changed; refresh before correcting it")
		}
	}
	now := s.now()
	rev := current + 1
	seq, err := nextNarrativeSeq(ctx, tx, taskID, itemID)
	if err != nil {
		return api.NarrativeLinkVersion{}, false, err
	}
	v := api.NarrativeLinkVersion{LinkID: id, TaskID: taskID, ItemID: itemID, Revision: rev, NarrativeSeq: seq, Action: req.Action, Relationship: req.Relationship, Target: req.Target, FeatureRevision: req.FeatureRevision, Reason: req.Reason, SourceTime: req.SourceTime, CreatedBy: narrativeActor(req.AgentID, req.RunID, by), CreatedAt: now}
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO narrative_links(id,task_id,item_id,latest_revision,created_at)VALUES(?,?,?,?,?)`, id, taskID, itemID, rev, ts(now))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE narrative_links SET latest_revision=? WHERE id=? AND latest_revision=?`, rev, id, current)
	}
	if err != nil {
		return v, false, err
	}
	var source any
	if req.SourceTime != nil {
		source = ts(*req.SourceTime)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_link_versions(link_id,revision,task_id,item_id,narrative_seq,action,relationship,target,feature_revision,reason,source_time,created_by,created_at)VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, rev, taskID, itemID, seq, req.Action, req.Relationship, marshalJSON(req.Target), req.FeatureRevision, req.Reason, source, marshalJSON(v.CreatedBy), ts(now))
	if err != nil {
		return v, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_timeline(task_id,item_id,seq,kind,object_id,object_version,relationship,source_time,created_at)VALUES(?,?,?,?,?,?,?,?,?)`, taskID, itemID, seq, "link", id, rev, req.Relationship, source, ts(now))
	if err != nil {
		return v, false, err
	}
	if _, err = saveNarrativeReceipt(ctx, tx, taskID, itemID, "link", req.RequestID, req.AgentID, payload, v, by, now); err != nil {
		return v, false, err
	}
	if err = tx.Commit(); err != nil {
		return v, false, err
	}
	s.notify(taskID)
	return v, false, nil
}

func (s *Store) ListNarrativeLinks(ctx context.Context, taskID, itemID, cursor string, limit int) (api.NarrativeLinkList, error) {
	if limit < 1 || limit > api.MaxNarrativePage {
		return api.NarrativeLinkList{}, api.ErrInvalid
	}
	c, err := decodeNarrativeCursor(cursor)
	if err != nil {
		return api.NarrativeLinkList{}, err
	}
	if _, err = s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeLinkList{}, err
	}
	if c.High == 0 {
		c.High, err = currentNarrativeSeq(s.db, ctx, taskID, itemID)
		if err != nil {
			return api.NarrativeLinkList{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+linkVersionCols+` FROM narrative_link_versions WHERE task_id=? AND item_id=? AND narrative_seq>? AND narrative_seq<=? ORDER BY narrative_seq LIMIT ?`, taskID, itemID, c.After, c.High, limit+1)
	if err != nil {
		return api.NarrativeLinkList{}, err
	}
	defer rows.Close()
	out := api.NarrativeLinkList{Links: []api.NarrativeLinkVersion{}}
	var last int64
	hasMore := false
	for rows.Next() {
		v, e := scanLinkVersion(rows)
		if e != nil {
			return out, e
		}
		if len(out.Links) == limit {
			hasMore = true
			break
		}
		next, fits := appendNarrativeMetadata(out.Links, v)
		if !fits {
			hasMore = true
			break
		}
		out.Links = next
		last = v.NarrativeSeq
	}
	if hasMore {
		out.NextCursor = encodeNarrativeCursor(narrativeCursor{c.High, last})
	}
	return out, rows.Err()
}

func scanCoverageVersion(row rowScanner) (api.NarrativeCoverageVersion, error) {
	var v api.NarrativeCoverageVersion
	var captured, gaps, evidence, assessor, actor, asof, created string
	var unknown int
	err := row.Scan(&v.CoverageID, &v.TaskID, &v.ItemID, &v.Revision, &v.NarrativeSeq, &v.Source, &v.Scope, &v.CaptureState, &captured, &gaps, &unknown, &asof, &v.Assessment, &v.AssessmentText, &evidence, &assessor, &actor, &created)
	if err != nil {
		return v, err
	}
	_ = json.Unmarshal([]byte(captured), &v.CapturedIDs)
	_ = json.Unmarshal([]byte(gaps), &v.KnownGaps)
	_ = json.Unmarshal([]byte(evidence), &v.EvidenceReferences)
	_ = json.Unmarshal([]byte(assessor), &v.AssessmentBy)
	_ = json.Unmarshal([]byte(actor), &v.CreatedBy)
	v.UnknownExtent = unknown == 1
	v.CreatedAt = parseTS(created)
	if asof != "" {
		t := parseTS(asof)
		v.AsOf = &t
	}
	return v, nil
}

const coverageVersionCols = `coverage_id,task_id,item_id,revision,narrative_seq,source,scope,capture_state,captured_ids,known_gaps,unknown_extent,COALESCE(as_of,''),assessment,assessment_text,evidence_refs,assessment_by,created_by,created_at`

func validateStringList(values []string) bool {
	if len(values) > 256 {
		return false
	}
	total := 0
	for _, v := range values {
		if !validNarrativeText(v, 1024, true) {
			return false
		}
		total += len(v)
	}
	return total <= 8192
}
func (s *Store) PutNarrativeCoverage(ctx context.Context, taskID, itemID string, req api.PutNarrativeCoverageRequest, by api.Caller) (api.NarrativeCoverageVersion, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(req.RequestID) || req.ExpectedRevision < 0 || (req.CoverageID != "" && !validNarrativeID(req.CoverageID, "ncov")) || !validNarrativeText(req.Source, 128, true) || !validNarrativeText(req.Scope, 4096, true) || !validCaptureState(req.CaptureState) || !validAssessment(req.Assessment) || !validNarrativeText(req.AssessmentText, 8192, false) || !validateStringList(req.CapturedIDs) || !validateStringList(req.KnownGaps) || len(req.EvidenceReferences) > 256 || !validNarrativeSender(req.AssessedBy, req.Assessment != "unverified") || (req.Assessment != "unverified" && len(req.EvidenceReferences) == 0) {
		return api.NarrativeCoverageVersion{}, false, api.ErrInvalid
	}
	payload := narrativePayloadHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.NarrativeCoverageVersion{}, false, err
	}
	defer tx.Rollback()
	if prior, ok, e := replayNarrativeReceipt[api.NarrativeCoverageVersion](tx, ctx, taskID, itemID, "coverage", req.RequestID, req.AgentID, payload, by); ok || e != nil {
		return prior, ok, e
	}
	if _, err = loadWritableFeature(tx, ctx, taskID, itemID); err != nil {
		return api.NarrativeCoverageVersion{}, false, err
	}
	if err = validateNarrativeActor(tx, ctx, taskID, req.AgentID, req.RunID); err != nil {
		return api.NarrativeCoverageVersion{}, false, err
	}
	if req.AssessedBy.AgentID != "" {
		if err = validateWorkItemAgent(tx, ctx, taskID, req.AssessedBy.AgentID); err != nil {
			return api.NarrativeCoverageVersion{}, false, err
		}
	}
	for _, ref := range req.EvidenceReferences {
		if err = validateNarrativeReference(tx, ctx, taskID, itemID, ref); err != nil {
			return api.NarrativeCoverageVersion{}, false, err
		}
	}
	id, current := req.CoverageID, int64(0)
	if id == "" {
		if req.ExpectedRevision != 0 {
			return api.NarrativeCoverageVersion{}, false, api.ErrInvalid
		}
		id = api.NewID("ncov")
	} else {
		err = tx.QueryRowContext(ctx, `SELECT latest_revision FROM narrative_coverage WHERE id=? AND task_id=? AND item_id=? AND source=?`, id, taskID, itemID, req.Source).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return api.NarrativeCoverageVersion{}, false, api.ErrNotFound
		}
		if err != nil {
			return api.NarrativeCoverageVersion{}, false, err
		}
		if current != req.ExpectedRevision {
			return api.NarrativeCoverageVersion{}, false, workItemConflict("coverage revision changed; refresh before correcting it")
		}
	}
	now := s.now()
	rev := current + 1
	seq, err := nextNarrativeSeq(ctx, tx, taskID, itemID)
	if err != nil {
		return api.NarrativeCoverageVersion{}, false, err
	}
	actor := narrativeActor(req.AgentID, req.RunID, by)
	v := api.NarrativeCoverageVersion{CoverageID: id, TaskID: taskID, ItemID: itemID, Revision: rev, NarrativeSeq: seq, Source: req.Source, Scope: req.Scope, CaptureState: req.CaptureState, CapturedIDs: req.CapturedIDs, KnownGaps: req.KnownGaps, UnknownExtent: req.UnknownExtent, AsOf: req.AsOf, Assessment: req.Assessment, AssessmentText: req.AssessmentText, EvidenceReferences: req.EvidenceReferences, AssessmentBy: req.AssessedBy, CreatedBy: actor, CreatedAt: now}
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO narrative_coverage(id,task_id,item_id,source,latest_revision,created_at)VALUES(?,?,?,?,?,?)`, id, taskID, itemID, req.Source, rev, ts(now))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE narrative_coverage SET latest_revision=? WHERE id=? AND latest_revision=?`, rev, id, current)
	}
	if err != nil {
		return v, false, err
	}
	var asof any
	if req.AsOf != nil {
		asof = ts(*req.AsOf)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_coverage_versions(coverage_id,revision,task_id,item_id,narrative_seq,source,scope,capture_state,captured_ids,known_gaps,unknown_extent,as_of,assessment,assessment_text,evidence_refs,assessment_by,created_by,created_at)VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, rev, taskID, itemID, seq, req.Source, req.Scope, req.CaptureState, marshalJSON(req.CapturedIDs), marshalJSON(req.KnownGaps), req.UnknownExtent, asof, req.Assessment, req.AssessmentText, marshalJSON(req.EvidenceReferences), marshalJSON(v.AssessmentBy), marshalJSON(v.CreatedBy), ts(now))
	if err != nil {
		return v, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_timeline(task_id,item_id,seq,kind,object_id,object_version,source,capture_state,created_at)VALUES(?,?,?,?,?,?,?,?,?)`, taskID, itemID, seq, "coverage", id, rev, req.Source, req.CaptureState, ts(now))
	if err != nil {
		return v, false, err
	}
	if _, err = saveNarrativeReceipt(ctx, tx, taskID, itemID, "coverage", req.RequestID, req.AgentID, payload, v, by, now); err != nil {
		return v, false, err
	}
	if err = tx.Commit(); err != nil {
		return v, false, err
	}
	s.notify(taskID)
	return v, false, nil
}

func (s *Store) ListNarrativeCoverage(ctx context.Context, taskID, itemID, cursor string, limit int) (api.NarrativeCoverageList, error) {
	if limit < 1 || limit > api.MaxNarrativePage {
		return api.NarrativeCoverageList{}, api.ErrInvalid
	}
	c, err := decodeNarrativeCursor(cursor)
	if err != nil {
		return api.NarrativeCoverageList{}, err
	}
	if _, err = s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeCoverageList{}, err
	}
	if c.High == 0 {
		c.High, err = currentNarrativeSeq(s.db, ctx, taskID, itemID)
		if err != nil {
			return api.NarrativeCoverageList{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+coverageVersionCols+` FROM narrative_coverage_versions WHERE task_id=? AND item_id=? AND narrative_seq>? AND narrative_seq<=? ORDER BY narrative_seq LIMIT ?`, taskID, itemID, c.After, c.High, limit+1)
	if err != nil {
		return api.NarrativeCoverageList{}, err
	}
	defer rows.Close()
	out := api.NarrativeCoverageList{Coverage: []api.NarrativeCoverageVersion{}}
	var last int64
	hasMore := false
	for rows.Next() {
		v, e := scanCoverageVersion(rows)
		if e != nil {
			return out, e
		}
		if len(out.Coverage) == limit {
			hasMore = true
			break
		}
		next, fits := appendNarrativeMetadata(out.Coverage, v)
		if !fits {
			hasMore = true
			break
		}
		out.Coverage = next
		last = v.NarrativeSeq
	}
	if hasMore {
		out.NextCursor = encodeNarrativeCursor(narrativeCursor{c.High, last})
	}
	return out, rows.Err()
}

func reportCanonical(sections api.NarrativeReportSections, refs []api.NarrativeReference) (string, string, error) {
	contentBytes := len(sections.RequestedOutcome) + len(sections.DeliveredWork) + len(sections.Verification) + len(sections.Limitations) + len(sections.RemainingWork)
	if contentBytes > api.MaxNarrativeContentBytes {
		return "", "", api.ErrInvalid
	}
	raw, err := json.Marshal(struct {
		Sections   api.NarrativeReportSections `json:"sections"`
		References []api.NarrativeReference    `json:"references"`
	}{sections, refs})
	if err != nil {
		return "", "", err
	}
	return string(raw), contentDigest(string(raw)), nil
}
func validReportSections(s api.NarrativeReportSections) bool {
	return validNarrativeText(s.RequestedOutcome, api.MaxNarrativeContentBytes, true) && validNarrativeText(s.DeliveredWork, api.MaxNarrativeContentBytes, true) && validNarrativeText(s.Verification, api.MaxNarrativeContentBytes, true) && validNarrativeText(s.Limitations, api.MaxNarrativeContentBytes, true) && validNarrativeText(s.RemainingWork, api.MaxNarrativeContentBytes, true)
}
func scanReportVersion(row rowScanner) (api.NarrativeReportVersion, error) {
	var v api.NarrativeReportVersion
	var sections, refs, actor, created string
	err := row.Scan(&v.ReportID, &v.TaskID, &v.ItemID, &v.Version, &v.NarrativeSeq, &v.ScopeRevision, &sections, &refs, &v.Digest, &actor, &created)
	if err != nil {
		return v, err
	}
	_ = json.Unmarshal([]byte(sections), &v.Sections)
	_ = json.Unmarshal([]byte(refs), &v.References)
	_ = json.Unmarshal([]byte(actor), &v.CreatedBy)
	v.CreatedAt = parseTS(created)
	return v, nil
}

const reportVersionCols = `report_id,task_id,item_id,version,narrative_seq,scope_revision,sections,refs,digest,created_by,created_at`

func (s *Store) PutNarrativeReport(ctx context.Context, taskID, itemID string, req api.PutNarrativeReportRequest, by api.Caller) (api.NarrativeReportVersion, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(req.RequestID) || req.ExpectedVersion < 0 || req.ScopeRevision < 1 || (req.ReportID != "" && !validNarrativeID(req.ReportID, "nrpt")) || !validReportSections(req.Sections) || len(req.References) < 1 || len(req.References) > 256 {
		return api.NarrativeReportVersion{}, false, api.ErrInvalid
	}
	canonical, digest, err := reportCanonical(req.Sections, req.References)
	if err != nil {
		return api.NarrativeReportVersion{}, false, err
	}
	payload := contentDigest(canonical + "\n" + narrativePayloadHash(req))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.NarrativeReportVersion{}, false, err
	}
	defer tx.Rollback()
	if prior, ok, e := replayNarrativeReceipt[api.NarrativeReportVersion](tx, ctx, taskID, itemID, "report", req.RequestID, req.AgentID, payload, by); ok || e != nil {
		return prior, ok, e
	}
	item, err := loadWritableFeature(tx, ctx, taskID, itemID)
	if err != nil {
		return api.NarrativeReportVersion{}, false, err
	}
	if req.ScopeRevision != item.ScopeRevision {
		return api.NarrativeReportVersion{}, false, fmt.Errorf("%w: current scope revision is %d", api.ErrNarrativeReportStale, item.ScopeRevision)
	}
	if err = validateNarrativeActor(tx, ctx, taskID, req.AgentID, req.RunID); err != nil {
		return api.NarrativeReportVersion{}, false, err
	}
	for _, ref := range req.References {
		if err = validateNarrativeReference(tx, ctx, taskID, itemID, ref); err != nil {
			return api.NarrativeReportVersion{}, false, err
		}
	}
	id, current := req.ReportID, int64(0)
	if id == "" {
		if req.ExpectedVersion != 0 {
			return api.NarrativeReportVersion{}, false, api.ErrInvalid
		}
		id = api.NewID("nrpt")
	} else {
		err = tx.QueryRowContext(ctx, `SELECT latest_version FROM narrative_reports WHERE id=? AND task_id=? AND item_id=?`, id, taskID, itemID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return api.NarrativeReportVersion{}, false, api.ErrNotFound
		}
		if err != nil {
			return api.NarrativeReportVersion{}, false, err
		}
		if current != req.ExpectedVersion {
			return api.NarrativeReportVersion{}, false, workItemConflict("report version changed; refresh before correcting it")
		}
	}
	now := s.now()
	version := current + 1
	seq, err := nextNarrativeSeq(ctx, tx, taskID, itemID)
	if err != nil {
		return api.NarrativeReportVersion{}, false, err
	}
	v := api.NarrativeReportVersion{ReportID: id, TaskID: taskID, ItemID: itemID, Version: version, NarrativeSeq: seq, ScopeRevision: req.ScopeRevision, Sections: req.Sections, References: req.References, Digest: digest, CreatedBy: narrativeActor(req.AgentID, req.RunID, by), CreatedAt: now}
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO narrative_reports(id,task_id,item_id,latest_version,created_at)VALUES(?,?,?,?,?)`, id, taskID, itemID, version, ts(now))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE narrative_reports SET latest_version=? WHERE id=? AND latest_version=?`, version, id, current)
	}
	if err != nil {
		return v, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_report_versions(report_id,version,task_id,item_id,narrative_seq,scope_revision,sections,refs,digest,created_by,created_at)VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, version, taskID, itemID, seq, req.ScopeRevision, marshalJSON(req.Sections), marshalJSON(req.References), digest, marshalJSON(v.CreatedBy), ts(now))
	if err != nil {
		return v, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_timeline(task_id,item_id,seq,kind,object_id,object_version,capture_state,created_at)VALUES(?,?,?,?,?,?,?,?)`, taskID, itemID, seq, "report", id, version, "stored-content", ts(now))
	if err != nil {
		return v, false, err
	}
	if _, err = saveNarrativeReceipt(ctx, tx, taskID, itemID, "report", req.RequestID, req.AgentID, payload, v, by, now); err != nil {
		return v, false, err
	}
	if err = tx.Commit(); err != nil {
		return v, false, err
	}
	s.notify(taskID)
	return v, false, nil
}

func (s *Store) GetNarrativeReportVersion(ctx context.Context, taskID, itemID, reportID string, version int64) (api.NarrativeReportVersion, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !validNarrativeID(reportID, "nrpt") || version < 1 {
		return api.NarrativeReportVersion{}, api.ErrInvalid
	}
	if _, err := s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeReportVersion{}, err
	}
	v, err := scanReportVersion(s.db.QueryRowContext(ctx, `SELECT `+reportVersionCols+` FROM narrative_report_versions WHERE task_id=? AND item_id=? AND report_id=? AND version=?`, taskID, itemID, reportID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return v, api.ErrNotFound
	}
	return v, err
}
func (s *Store) ListNarrativeReports(ctx context.Context, taskID, itemID, cursor string, limit int) (api.NarrativeReportList, error) {
	if limit < 1 || limit > api.MaxNarrativePage {
		return api.NarrativeReportList{}, api.ErrInvalid
	}
	c, err := decodeNarrativeCursor(cursor)
	if err != nil {
		return api.NarrativeReportList{}, err
	}
	if _, err = s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeReportList{}, err
	}
	if c.High == 0 {
		c.High, err = currentNarrativeSeq(s.db, ctx, taskID, itemID)
		if err != nil {
			return api.NarrativeReportList{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT report_id,version,scope_revision,digest,created_at,narrative_seq FROM narrative_report_versions WHERE task_id=? AND item_id=? AND narrative_seq>? AND narrative_seq<=? ORDER BY narrative_seq LIMIT ?`, taskID, itemID, c.After, c.High, limit+1)
	if err != nil {
		return api.NarrativeReportList{}, err
	}
	defer rows.Close()
	out := api.NarrativeReportList{Reports: []api.NarrativeReportSummary{}}
	var last int64
	hasMore := false
	for rows.Next() {
		var v api.NarrativeReportSummary
		var created string
		var seq int64
		if err = rows.Scan(&v.ReportID, &v.Version, &v.ScopeRevision, &v.Digest, &created, &seq); err != nil {
			return out, err
		}
		v.CreatedAt = parseTS(created)
		if len(out.Reports) == limit {
			hasMore = true
			break
		}
		next, fits := appendNarrativeMetadata(out.Reports, v)
		if !fits {
			hasMore = true
			break
		}
		out.Reports = next
		last = seq
	}
	if hasMore {
		out.NextCursor = encodeNarrativeCursor(narrativeCursor{c.High, last})
	}
	return out, rows.Err()
}

func validateNarrativeCompletion(ctx context.Context, q queryRower, item api.WorkItem, pin *api.NarrativeReportPin) error {
	if pin == nil || !validNarrativeID(pin.ReportID, "nrpt") || pin.Version < 1 || pin.ScopeRevision < 1 || pin.Digest == "" {
		return api.ErrNarrativeReportRequired
	}
	var scope int64
	var digest, sectionsRaw, refsRaw string
	err := q.QueryRowContext(ctx, `SELECT scope_revision,digest,sections,refs FROM narrative_report_versions WHERE task_id=? AND item_id=? AND report_id=? AND version=?`, item.TaskID, item.ID, pin.ReportID, pin.Version).Scan(&scope, &digest, &sectionsRaw, &refsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ErrNarrativeReportRequired
	}
	if err != nil {
		return err
	}
	var sections api.NarrativeReportSections
	var refs []api.NarrativeReference
	if json.Unmarshal([]byte(sectionsRaw), &sections) != nil || json.Unmarshal([]byte(refsRaw), &refs) != nil {
		return api.ErrNarrativeReportStale
	}
	_, verifiedDigest, verifyErr := reportCanonical(sections, refs)
	if verifyErr != nil || verifiedDigest != digest || scope != item.ScopeRevision || pin.ScopeRevision != scope || pin.Digest != digest {
		return api.ErrNarrativeReportStale
	}
	var latest int64
	if err = q.QueryRowContext(ctx, `SELECT latest_version FROM narrative_reports WHERE id=?`, pin.ReportID).Scan(&latest); err != nil {
		return err
	}
	if latest != pin.Version {
		return api.ErrNarrativeReportStale
	}
	var currentID string
	var currentVersion int64
	if err = q.QueryRowContext(ctx, `SELECT report_id,version FROM narrative_report_versions WHERE task_id=? AND item_id=? ORDER BY narrative_seq DESC LIMIT 1`, item.TaskID, item.ID).Scan(&currentID, &currentVersion); err != nil {
		return err
	}
	if currentID != pin.ReportID || currentVersion != pin.Version {
		return api.ErrNarrativeReportStale
	}
	return nil
}
func insertNarrativeCompletion(ctx context.Context, tx *sql.Tx, item api.WorkItem, pin api.NarrativeReportPin, agentID, runID string, by api.Caller, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO narrative_completion_pins(task_id,item_id,item_revision,report_id,report_version,report_digest,scope_revision,actor,created_at)VALUES(?,?,?,?,?,?,?,?,?)`, item.TaskID, item.ID, item.Revision, pin.ReportID, pin.Version, pin.Digest, pin.ScopeRevision, marshalJSON(narrativeActor(agentID, runID, by)), ts(now))
	if err != nil {
		return err
	}
	seq, err := nextNarrativeSeq(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO narrative_timeline(task_id,item_id,seq,kind,object_id,object_version,capture_state,created_at)VALUES(?,?,?,?,?,?,?,?)`, item.TaskID, item.ID, seq, "completion", pin.ReportID, pin.Version, "stored-content", ts(now))
	return err
}

func (s *Store) ListNarrativeTimeline(ctx context.Context, taskID, itemID, cursor string, limit int, kind, source, relationship, captureState, sourceFrom, sourceTo string) (api.NarrativeTimelinePage, error) {
	if limit < 1 || limit > api.MaxNarrativePage {
		return api.NarrativeTimelinePage{}, api.ErrInvalid
	}
	c, err := decodeNarrativeCursor(cursor)
	if err != nil {
		return api.NarrativeTimelinePage{}, err
	}
	if _, err = s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.NarrativeTimelinePage{}, err
	}
	if c.High == 0 {
		c.High, err = currentNarrativeSeq(s.db, ctx, taskID, itemID)
		if err != nil {
			return api.NarrativeTimelinePage{}, err
		}
	}
	query := `SELECT seq,kind,object_id,object_version,source,relationship,capture_state,COALESCE(source_time,''),created_at FROM narrative_timeline WHERE task_id=? AND item_id=? AND seq>? AND seq<=?`
	args := []any{taskID, itemID, c.After, c.High}
	for _, f := range []struct{ v, col string }{{kind, "kind"}, {source, "source"}, {relationship, "relationship"}, {captureState, "capture_state"}} {
		if f.v != "" {
			query += " AND " + f.col + "=?"
			args = append(args, f.v)
		}
	}
	if sourceFrom != "" {
		parsed, e := time.Parse(time.RFC3339Nano, sourceFrom)
		if e != nil {
			return api.NarrativeTimelinePage{}, api.ErrInvalid
		}
		query += " AND source_time>=?"
		args = append(args, ts(parsed))
	}
	if sourceTo != "" {
		parsed, e := time.Parse(time.RFC3339Nano, sourceTo)
		if e != nil {
			return api.NarrativeTimelinePage{}, api.ErrInvalid
		}
		query += " AND source_time<=?"
		args = append(args, ts(parsed))
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return api.NarrativeTimelinePage{}, err
	}
	defer rows.Close()
	out := api.NarrativeTimelinePage{Entries: []api.NarrativeTimelineEntry{}, HighWatermark: c.High}
	var last int64
	hasMore := false
	for rows.Next() {
		var v api.NarrativeTimelineEntry
		var sourceTime, created string
		if err = rows.Scan(&v.Seq, &v.Kind, &v.ObjectID, &v.Version, &v.Source, &v.Relationship, &v.CaptureState, &sourceTime, &created); err != nil {
			return out, err
		}
		v.CreatedAt = parseTS(created)
		if sourceTime != "" {
			t := parseTS(sourceTime)
			v.SourceTime = &t
		}
		if len(out.Entries) == limit {
			hasMore = true
			break
		}
		next, fits := appendNarrativeMetadata(out.Entries, v)
		if !fits {
			hasMore = true
			break
		}
		out.Entries = next
		last = v.Seq
	}
	if hasMore {
		out.Cursor = encodeNarrativeCursor(narrativeCursor{c.High, last})
	}
	return out, rows.Err()
}

func (s *Store) GetNarrativeReceipt(ctx context.Context, taskID, itemID, operation, requestID, agentID string, by api.Caller) (api.NarrativeReceipt, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(requestID) || !validNarrativeText(operation, 32, true) || (agentID != "" && !api.ValidID(agentID, "agt")) {
		return api.NarrativeReceipt{}, api.ErrInvalid
	}
	r, _, err := findNarrativeReceipt(s.db, ctx, taskID, itemID, operation, requestID, agentID, by)
	return r, err
}

func (s *Store) GetNarrativeOverview(ctx context.Context, taskID, itemID string) (api.NarrativeOverview, error) {
	return s.GetNarrativeOverviewPage(ctx, taskID, itemID, "", api.DefaultNarrativePage)
}

func (s *Store) GetNarrativeOverviewPage(ctx context.Context, taskID, itemID, cursor string, limit int) (api.NarrativeOverview, error) {
	if limit < 1 || limit > api.MaxNarrativePage {
		return api.NarrativeOverview{}, api.ErrInvalid
	}
	c, err := decodeNarrativeCursor(cursor)
	if err != nil {
		return api.NarrativeOverview{}, err
	}
	item, err := s.GetWorkItem(ctx, taskID, itemID)
	if err != nil {
		return api.NarrativeOverview{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.NarrativeOverview{}, err
	}
	defer tx.Rollback()
	if c.High == 0 {
		c.High, err = currentNarrativeSeq(tx, ctx, taskID, itemID)
		if err != nil {
			return api.NarrativeOverview{}, err
		}
	}
	history, err := historyCoverage(ctx, tx, item)
	if err != nil {
		return api.NarrativeOverview{}, err
	}
	out := api.NarrativeOverview{Item: item, History: history, CompletionReport: item.CompletionReport, Coverage: []api.NarrativeCoverageVersion{}, DefaultGaps: []api.NarrativeCoverageVersion{}}
	v, err := scanReportVersion(tx.QueryRowContext(ctx, `SELECT `+reportVersionCols+` FROM narrative_report_versions WHERE task_id=? AND item_id=? ORDER BY narrative_seq DESC LIMIT 1`, taskID, itemID))
	if err == nil {
		out.LatestReport = &api.NarrativeReportSummary{ReportID: v.ReportID, Version: v.Version, ScopeRevision: v.ScopeRevision, Digest: v.Digest, CreatedAt: v.CreatedAt}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+coverageVersionCols+` FROM narrative_coverage_versions v WHERE task_id=? AND item_id=? AND narrative_seq>? AND narrative_seq<=? AND revision=(SELECT max(v2.revision) FROM narrative_coverage_versions v2 WHERE v2.coverage_id=v.coverage_id AND v2.narrative_seq<=?) ORDER BY narrative_seq LIMIT ?`, taskID, itemID, c.After, c.High, c.High, limit+1)
	if err != nil {
		return out, err
	}
	var last int64
	hasMore := false
	for rows.Next() {
		c, e := scanCoverageVersion(rows)
		if e != nil {
			rows.Close()
			return out, e
		}
		if len(out.Coverage) == limit {
			hasMore = true
			break
		}
		next, fits := appendNarrativeMetadata(out.Coverage, c)
		if !fits {
			hasMore = true
			break
		}
		out.Coverage = next
		last = c.NarrativeSeq
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	if err = rows.Close(); err != nil {
		return out, err
	}
	if hasMore {
		out.CoverageNextCursor = encodeNarrativeCursor(narrativeCursor{c.High, last})
	}
	for _, source := range []string{"pr", "ci"} {
		var seen int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM narrative_coverage_versions WHERE task_id=? AND item_id=? AND source=? AND narrative_seq<=?`, taskID, itemID, source, c.High).Scan(&seen); err != nil {
			return out, err
		}
		if seen == 0 {
			out.DefaultGaps = append(out.DefaultGaps, api.NarrativeCoverageVersion{Source: source, Scope: "No capture declaration has been submitted.", CaptureState: "not-ingested", KnownGaps: []string{"source history is not captured"}, UnknownExtent: true, Assessment: "unverified"})
		}
	}
	out.LegacyReportMissing = item.Kind == "feature" && item.Status == "done" && item.CompletionReport == nil
	return out, tx.Commit()
}
