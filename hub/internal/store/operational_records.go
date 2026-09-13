package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

func migrateOperationalRecords(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS operational_records (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), version INTEGER NOT NULL,
 state TEXT NOT NULL, authority_key TEXT UNIQUE);
 CREATE TABLE IF NOT EXISTS operational_record_versions (
 record_id TEXT NOT NULL REFERENCES operational_records(id), version INTEGER NOT NULL,
 data BLOB NOT NULL, PRIMARY KEY(record_id,version));`)
	return err
}
func operationalKind(k string) bool {
	switch k {
	case "instruction", "finding", "candidate", "verification", "result", "acceptance":
		return true
	}
	return false
}
func operationalDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func operationalHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}
func operationalText(s string) bool { return strings.TrimSpace(s) != "" && api.ValidText(s, 8192) }
func operationalStrings(a []string) bool {
	if len(a) == 0 || len(a) > 32 {
		return false
	}
	for _, s := range a {
		if !operationalText(s) {
			return false
		}
	}
	return true
}
func operationalShape(d api.OperationalData) bool {
	if !operationalKind(d.Kind) || len(d.Sources) > 16 {
		return false
	}
	count := 0
	match := false
	for k, present := range map[string]bool{"instruction": d.Instruction != nil, "finding": d.Finding != nil, "candidate": d.Candidate != nil, "verification": d.Verification != nil, "result": d.Result != nil, "acceptance": d.Acceptance != nil} {
		if present {
			count++
			match = k == d.Kind
		}
	}
	raw, _ := json.Marshal(d)
	return count <= 1 && (count == 0 || match) && len(raw) <= 65536
}
func getOperationalRecord(q queryRower, ctx context.Context, task, id string, version int64) (api.OperationalRecord, error) {
	var out api.OperationalRecord
	var raw []byte
	query := `SELECT v.data FROM operational_records r JOIN operational_record_versions v ON v.record_id=r.id AND v.version=r.version WHERE r.task_id=? AND r.id=?`
	args := []any{task, id}
	if version > 0 {
		query = `SELECT v.data FROM operational_records r JOIN operational_record_versions v ON v.record_id=r.id WHERE r.task_id=? AND r.id=? AND v.version=?`
		args = append(args, version)
	}
	err := q.QueryRowContext(ctx, query, args...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, api.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}
func (s *Store) GetOperationalRecord(ctx context.Context, task, id string, version int64) (api.OperationalRecord, error) {
	if !api.ValidID(task, "tsk") || !validDeliveryID(id, "opr") || version < 0 {
		return api.OperationalRecord{}, api.ErrInvalid
	}
	return getOperationalRecord(s.db, ctx, task, id, version)
}

// Uses the first core's shared receipt table, with distinct operation names.
// Proposals have no delivery event: they are retained input, never assignments.
func operationalReplay(q queryRower, ctx context.Context, task, op, subject, agent, run, key, hash string, by api.Caller) (api.OperationalMutation, error) {
	var out api.OperationalMutation
	var oldSubject, oldHash string
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT subject_id,payload_hash,response_json FROM delivery_operation_receipts WHERE task_id=? AND operation=? AND actor_agent_id=? AND actor_run_id=? AND by_node=? AND by_user=? AND request_id=?`, task, op, agent, run, by.Node, by.User, key).Scan(&oldSubject, &oldHash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, api.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if subject != oldSubject || hash != oldHash {
		return out, workItemConflict("operational retry key has different subject or payload")
	}
	err = json.Unmarshal(raw, &out)
	out.Replay = true
	return out, err
}
func saveOperational(ctx context.Context, tx *sql.Tx, task, op, subject, agent, run, key, hash string, by api.Caller, out *api.OperationalMutation) error {
	raw, err := json.Marshal(out.Record)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operational_record_versions(record_id,version,data) VALUES(?,?,?)`, out.Record.ID, out.Record.Version, raw); err != nil {
		return err
	}
	out.Receipt = api.DeliveryReceipt{ID: api.NewID("drr"), RequestID: key, Operation: op, CreatedAt: out.Record.CreatedAt}
	raw, err = json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_operation_receipts(receipt_id,task_id,operation,subject_id,actor_agent_id,actor_run_id,by_node,by_user,request_id,payload_hash,response_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, out.Receipt.ID, task, op, subject, agent, run, by.Node, by.User, key, hash, raw, ts(out.Record.CreatedAt))
	return err
}
func operationalActor(q queryRower, ctx context.Context, task, agent, run string) error {
	if agent == "" && run == "" {
		return nil
	}
	if !api.ValidID(agent, "agt") || !validRunID(run) {
		return api.ErrInvalid
	}
	var current, status string
	err := q.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE task_id=? AND id=?`, task, agent).Scan(&current, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ErrNotFound
	}
	if err != nil {
		return err
	}
	if current != run || status == api.AgentClosed || status == api.AgentExited || status == api.AgentRetired {
		return workItemConflict("operational actor is not an active exact run")
	}
	return nil
}
func openOperationalTask(q queryRower, ctx context.Context, task string) error {
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, task).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != api.TaskOpen {
		return api.ErrClosed
	}
	return nil
}
func (s *Store) ProposeOperationalRecord(ctx context.Context, task string, req api.ProposeOperationalRecordRequest, by api.Caller) (api.OperationalMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.OperationalMutation
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) || !operationalShape(req.Data) || req.ExpectedVersion < 0 || (req.RecordID != "" && !validDeliveryID(req.RecordID, "opr")) || ((req.RecordID == "") != (req.ExpectedVersion == 0)) {
		return out, api.ErrInvalid
	}
	op := "operational.propose"
	hash := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, e := operationalReplay(tx, ctx, task, op, req.RecordID, req.AgentID, req.RunID, req.RequestID, hash, by); e == nil {
		return replay, nil
	} else if !errors.Is(e, api.ErrNotFound) {
		return out, e
	}
	if err = openOperationalTask(tx, ctx, task); err != nil {
		return out, err
	}
	if err = operationalActor(tx, ctx, task, req.AgentID, req.RunID); err != nil {
		return out, err
	}
	id := req.RecordID
	if id == "" {
		id = api.NewID("opr")
		_, err = tx.ExecContext(ctx, `INSERT INTO operational_records(id,task_id,version,state) VALUES(?,?,1,'proposed')`, id, task)
	} else {
		prior, e := getOperationalRecord(tx, ctx, task, id, 0)
		if e != nil {
			return out, e
		}
		if prior.Version != req.ExpectedVersion || prior.State != "proposed" {
			return out, workItemConflict("proposal version changed or record already committed")
		}
		if prior.ActorAgentID != req.AgentID || prior.ActorRunID != req.RunID || prior.By != by {
			return out, workItemConflict("only original proposal actor may revise it")
		}
		_, err = tx.ExecContext(ctx, `UPDATE operational_records SET version=version+1 WHERE id=?`, id)
	}
	if err != nil {
		return out, err
	}
	out.Record = api.OperationalRecord{ID: id, TaskID: task, Version: req.ExpectedVersion + 1, State: "proposed", Data: req.Data, ActorAgentID: req.AgentID, ActorRunID: req.RunID, By: by, CreatedAt: s.now()}
	if err = saveOperational(ctx, tx, task, op, req.RecordID, req.AgentID, req.RunID, req.RequestID, hash, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(task)
	return out, nil
}
func operationalReference(q queryRower, ctx context.Context, parent api.OperationalRecord, ref api.OperationalReference, kind string) (api.OperationalRecord, error) {
	if !validDeliveryID(ref.ID, "opr") || ref.Version < 1 {
		return api.OperationalRecord{}, api.ErrInvalid
	}
	r, err := getOperationalRecord(q, ctx, parent.TaskID, ref.ID, ref.Version)
	if err != nil {
		return r, err
	}
	a, b := r.Data, parent.Data
	if r.State != "committed" || a.Kind != kind || a.ItemTaskID != b.ItemTaskID || a.ItemID != b.ItemID || a.ItemRevision != b.ItemRevision || a.ScopeRevision != b.ScopeRevision || a.DeliveryID != b.DeliveryID || a.Generation != b.Generation || a.Epoch != b.Epoch {
		return r, workItemConflict("referenced operational record is not committed for this exact instruction epoch")
	}
	return r, nil
}
func operationalArtifact(q queryRower, ctx context.Context, d api.OperationalData, pin api.OperationalArtifact) (api.NarrativeArtifactVersion, error) {
	var a api.NarrativeArtifactVersion
	if pin.Version < 1 || !operationalHex(pin.Digest, 64) {
		return a, api.ErrInvalid
	}
	// The normal artifact reader validates item association; use its transactional equivalent.
	return loadOperationalArtifact(q, ctx, d, pin)
}
func loadOperationalArtifact(q queryRower, ctx context.Context, d api.OperationalData, pin api.OperationalArtifact) (api.NarrativeArtifactVersion, error) {
	a, err := scanArtifactVersion(q.QueryRowContext(ctx, `SELECT `+artifactVersionCols+` FROM narrative_artifact_versions v JOIN narrative_artifacts a ON a.id=v.artifact_id WHERE v.task_id=? AND v.item_id=? AND v.artifact_id=? AND v.version=?`, d.ItemTaskID, d.ItemID, pin.ID, pin.Version))
	if errors.Is(err, sql.ErrNoRows) {
		return a, api.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	if a.CaptureState != "stored-content" || a.Content == "" || a.ContentDigest != pin.Digest || operationalDigest([]byte(a.Content)) != pin.Digest {
		return a, workItemConflict("artifact bytes unavailable or digest differs")
	}
	return a, nil
}
func operationalSources(q queryRower, ctx context.Context, d api.OperationalData) ([]api.OperationalSourceEvidence, error) {
	if len(d.Sources) == 0 {
		return nil, workItemConflict("committed operational records require source evidence")
	}
	var out []api.OperationalSourceEvidence
	seen := map[api.MessageReference]bool{}
	for _, src := range d.Sources {
		if src.Message.TaskID != d.ItemTaskID || src.Message.Seq < 1 || !operationalHex(src.TextDigest, 64) || seen[src.Message] {
			return nil, api.ErrInvalid
		}
		seen[src.Message] = true
		m, err := loadMessage(q, ctx, src.Message.TaskID, src.Message.Seq)
		if err != nil {
			return nil, err
		}
		if operationalDigest([]byte(m.Text)) != src.TextDigest || !hasDeliveryItemLink(m, d.ItemTaskID, d.ItemID, d.ItemRevision) {
			return nil, workItemConflict("source digest or immutable item association does not match")
		}
		out = append(out, api.OperationalSourceEvidence{OperationalSource: src, Author: m.From, Text: m.Text})
	}
	return out, nil
}
func strictOperationalJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return api.ErrInvalid
	}
	return nil
}
func validateOperationalContent(q queryRower, ctx context.Context, r api.OperationalRecord) (string, error) {
	d := r.Data
	instruction := func(ref api.OperationalReference) (api.OperationalRecord, error) {
		return operationalReference(q, ctx, r, ref, "instruction")
	}
	switch d.Kind {
	case "instruction":
		p := d.Instruction
		if p == nil || !operationalText(p.Objective) || !operationalStrings(p.Scope) || len(p.Criteria) == 0 || len(p.Criteria) > 32 {
			return "", api.ErrInvalid
		}
		seen := map[string]bool{}
		for _, c := range p.Criteria {
			if !validRequestID(c.ID) || !operationalText(c.Description) || seen[c.ID] {
				return "", api.ErrInvalid
			}
			seen[c.ID] = true
		}
		return "instruction:" + d.DeliveryID + fmt.Sprintf(":%d:%d", d.Generation, d.Epoch), nil
	case "finding":
		p := d.Finding
		if p == nil || !operationalText(p.Observation) || !operationalStrings(p.Reproduction) {
			return "", api.ErrInvalid
		}
		_, err := instruction(p.Instruction)
		return "", err
	case "candidate":
		p := d.Candidate
		if p == nil || !operationalHex(p.Commit, 40) || len(p.Findings) > 32 {
			return "", api.ErrInvalid
		}
		if _, err := instruction(p.Instruction); err != nil {
			return "", err
		}
		artifact, err := operationalArtifact(q, ctx, d, p.Artifact)
		if err != nil {
			return "", err
		}
		var manifest api.OperationalCandidateEvidence
		if strictOperationalJSON([]byte(artifact.Content), &manifest) != nil || manifest.Version != 1 || manifest.Commit != p.Commit || len(manifest.Files) == 0 || len(manifest.Files) > 256 {
			return "", workItemConflict("candidate commit does not match a complete retained manifest")
		}
		paths := map[string]bool{}
		for _, f := range manifest.Files {
			if !operationalText(f.Path) || !operationalHex(f.Digest, 64) || paths[f.Path] {
				return "", api.ErrInvalid
			}
			paths[f.Path] = true
		}
		seen := map[api.OperationalReference]bool{}
		for _, ref := range p.Findings {
			f, err := operationalReference(q, ctx, r, ref, "finding")
			if err != nil {
				return "", err
			}
			if f.Data.Finding.Instruction != p.Instruction || seen[ref] {
				return "", workItemConflict("finding belongs to a different instruction or is duplicated")
			}
			seen[ref] = true
		}
		return "", nil
	case "verification":
		p := d.Verification
		if p == nil || !validRequestID(p.TestID) || !operationalStrings(p.Command) || (p.Outcome != "pass" && p.Outcome != "fail") {
			return "", api.ErrInvalid
		}
		candidate, err := operationalReference(q, ctx, r, p.Candidate, "candidate")
		if err != nil {
			return "", err
		}
		ins, err := instruction(candidate.Data.Candidate.Instruction)
		if err != nil {
			return "", err
		}
		found := false
		for _, c := range ins.Data.Instruction.Criteria {
			if c.ID == p.CriterionID {
				found = true
			}
		}
		if !found {
			return "", workItemConflict("verification does not address an instruction criterion")
		}
		artifact, err := operationalArtifact(q, ctx, d, p.Evidence)
		if err != nil {
			return "", err
		}
		var ev api.OperationalTestEvidence
		if strictOperationalJSON([]byte(artifact.Content), &ev) != nil || ev.Version != 1 || ev.Candidate != p.Candidate || ev.Commit != candidate.Data.Candidate.Commit || ev.CandidateDigest != candidate.Data.Candidate.Artifact.Digest || ev.CriterionID != p.CriterionID || ev.TestID != p.TestID || !reflect.DeepEqual(ev.Command, p.Command) || ev.Outcome != p.Outcome || ev.AgentID != r.ActorAgentID || ev.RunID != r.ActorRunID || artifact.OriginalAuthor.AgentID != r.ActorAgentID {
			return "", workItemConflict("test evidence does not match candidate, criterion, command, outcome and exact actor")
		}
		return "", nil
	case "result":
		p := d.Result
		if p == nil || !operationalText(p.Summary) || len(p.Verifications) == 0 || len(p.Verifications) > 64 {
			return "", api.ErrInvalid
		}
		candidate, err := operationalReference(q, ctx, r, p.Candidate, "candidate")
		if err != nil {
			return "", err
		}
		ins, err := instruction(candidate.Data.Candidate.Instruction)
		if err != nil {
			return "", err
		}
		covered := map[string]bool{}
		for _, ref := range p.Verifications {
			v, e := operationalReference(q, ctx, r, ref, "verification")
			if e != nil {
				return "", e
			}
			if v.Data.Verification.Candidate != p.Candidate || covered[v.Data.Verification.CriterionID] {
				return "", workItemConflict("verification candidate differs or criterion has conflicting duplicate outcomes")
			}
			covered[v.Data.Verification.CriterionID] = true
		}
		if len(covered) != len(ins.Data.Instruction.Criteria) {
			return "", workItemConflict("result does not cover every bounded acceptance criterion")
		}
		return "result:" + d.DeliveryID + fmt.Sprintf(":%d:%d", d.Generation, d.Epoch), nil
	case "acceptance":
		p := d.Acceptance
		if p == nil || !operationalText(p.Reason) || (p.Decision != "accepted" && p.Decision != "rejected") {
			return "", api.ErrInvalid
		}
		result, err := operationalReference(q, ctx, r, p.Result, "result")
		if err != nil {
			return "", err
		}
		if p.Decision == "accepted" {
			for _, ref := range result.Data.Result.Verifications {
				v, e := operationalReference(q, ctx, r, ref, "verification")
				if e != nil {
					return "", e
				}
				if v.Data.Verification.Outcome != "pass" {
					return "", workItemConflict("failed verification cannot support acceptance")
				}
			}
		}
		return "acceptance:" + p.Result.ID, nil
	}
	return "", api.ErrInvalid
}
func (s *Store) CommitOperationalRecord(ctx context.Context, task, id string, req api.CommitOperationalRecordRequest, by api.Caller) (api.OperationalMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.OperationalMutation
	if !api.ValidID(task, "tsk") || !validDeliveryID(id, "opr") || !validRequestID(req.RequestID) || req.ExpectedVersion < 1 {
		return out, api.ErrInvalid
	}
	op := "operational.commit"
	hash := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, e := operationalReplay(tx, ctx, task, op, id, req.AgentID, req.RunID, req.RequestID, hash, by); e == nil {
		return replay, nil
	} else if !errors.Is(e, api.ErrNotFound) {
		return out, e
	}
	if err = openOperationalTask(tx, ctx, task); err != nil {
		return out, err
	}
	if err = operationalActor(tx, ctx, task, req.AgentID, req.RunID); err != nil {
		return out, err
	}
	r, err := getOperationalRecord(tx, ctx, task, id, 0)
	if err != nil {
		return out, err
	}
	if r.State != "proposed" || r.Version != req.ExpectedVersion {
		return out, workItemConflict("operational record changed or already committed")
	}
	d := r.Data
	if !api.ValidID(d.ItemTaskID, "tsk") || !api.ValidID(d.ItemID, "wi") || d.ItemRevision < 1 || d.ScopeRevision < 1 || !validDeliveryID(d.DeliveryID, "dly") || d.Generation < 1 || d.Epoch < 1 {
		return out, api.ErrInvalid
	}
	item, err := getWorkItem(tx, ctx, d.ItemTaskID, d.ItemID)
	if err != nil {
		return out, err
	}
	if item.Revision != d.ItemRevision || item.ScopeRevision != d.ScopeRevision || item.Status == "done" || item.Status == "dismissed" {
		return out, workItemConflict("operational item revision, scope or status is stale")
	}
	delivery, err := getRequiredDeliveryRow(tx, ctx, task, d.DeliveryID)
	if err != nil {
		return out, err
	}
	if !delivery.Current || delivery.Generation != d.Generation || delivery.ExecutionEpoch != d.Epoch || delivery.ItemTaskID != d.ItemTaskID || delivery.ItemID != d.ItemID || delivery.ItemRevision != d.ItemRevision {
		return out, workItemConflict("operational directive binding, generation or epoch is stale")
	}
	if err = validateDeliveryBinding(tx, ctx, delivery); err != nil {
		return out, err
	}
	if err = operationalActor(tx, ctx, task, delivery.AgentID, delivery.RunID); err != nil {
		return out, err
	}
	if d.Kind == "instruction" || d.Kind == "acceptance" {
		err = validateDeliveryProducer(tx, ctx, task, req.AgentID, req.RunID)
	} else if d.Kind == "verification" {
		if req.AgentID == "" {
			err = workItemConflict("verification requires a recorded exact test actor")
		} else {
			var b *api.AgentWorkItemBinding
			b, err = loadAgentWorkItemBinding(tx, ctx, req.AgentID, req.RunID)
			if err == nil && (b == nil || b.ItemTaskID != d.ItemTaskID || b.ItemID != d.ItemID || b.ItemRevision != d.ItemRevision) {
				err = workItemConflict("test actor is not bound to this item revision")
			}
		}
	} else if req.AgentID != delivery.AgentID || req.RunID != delivery.RunID {
		err = workItemConflict("operational submission requires the exact directive worker")
	}
	if err != nil {
		return out, err
	}
	if d.Kind == "instruction" {
		if delivery.Phase != api.DeliveryUnacknowledged {
			return out, workItemConflict("instruction must precede acknowledgment")
		}
	} else if d.Kind == "acceptance" {
		if delivery.Phase != api.DeliveryResult {
			return out, workItemConflict("acceptance requires submitted result")
		}
	} else if delivery.Phase != api.DeliveryAcknowledged && delivery.Phase != api.DeliveryProgressing {
		return out, workItemConflict("operational work requires acknowledged active execution")
	}
	r.ActorAgentID, r.ActorRunID, r.By = req.AgentID, req.RunID, by
	r.Sources, err = operationalSources(tx, ctx, d)
	if err != nil {
		return out, err
	}
	authorityKey, err := validateOperationalContent(tx, ctx, r)
	if err != nil {
		return out, err
	}
	if authorityKey != "" {
		var existing string
		e := tx.QueryRowContext(ctx, `SELECT id FROM operational_records WHERE authority_key=?`, authorityKey).Scan(&existing)
		if e == nil {
			return out, workItemConflict("authority record already committed: " + existing)
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
	}
	r.Version++
	r.State = "committed"
	r.CreatedAt = s.now()
	out.Record = r
	if _, err = tx.ExecContext(ctx, `UPDATE operational_records SET version=?,state='committed',authority_key=? WHERE id=?`, r.Version, nullable(authorityKey), id); err != nil {
		return out, err
	}
	if d.Kind == "result" {
		if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,result_text=?,updated_at=? WHERE id=?`, api.DeliveryResult, d.Result.Summary, ts(r.CreatedAt), delivery.ID); err != nil {
			return out, err
		}
	}
	event, err := insertDeliveryEvent(ctx, tx, delivery, "operational_"+d.Kind, req.RequestID, req.AgentID, req.RunID, map[string]any{"recordId": id, "recordVersion": r.Version, "generation": d.Generation, "executionEpoch": d.Epoch}, by, r.CreatedAt)
	if err != nil {
		return out, err
	}
	out.Event = &event
	if err = saveOperational(ctx, tx, task, op, id, req.AgentID, req.RunID, req.RequestID, hash, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(task)
	return out, nil
}
