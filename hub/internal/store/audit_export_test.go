package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestAuditExportFrozenReplayChunksClosedAndExpiration(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	caller := api.Caller{Node: "test-node", User: "test-user"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic export"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	source, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "before snapshot", RequestID: "post-before"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "frozen item", Priority: "normal", SourceMessageSeq: source.Seq, RequestID: "item-before"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	work, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "frozen work link", AuditKind: api.MessageAuditWork, RequestID: "work-before", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: task.ID, Seq: source.Seq}}, caller)
	if err != nil {
		t.Fatal(err)
	}
	req := api.CreateAuditExportRequest{RequestID: "snapshot-one", FormatVersion: 2}
	created, err := s.CreateAuditExport(ctx, task.ID, req, caller)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Available || created.Replay || created.ByteCount < 1 {
		t.Fatalf("unexpected metadata: %+v", created)
	}
	if _, err = s.GetAuditExportChunk(ctx, task.ID, created.ID, 0, api.MaxAuditExportChunkBytes+1, caller); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("oversized chunk err=%v", err)
	}
	if _, err = s.GetAuditExportChunk(ctx, task.ID, created.ID, created.ByteCount+1, 17, caller); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("past-end chunk err=%v", err)
	}
	if _, err = s.GetAuditExportChunk(ctx, task.ID, created.ID, 0, 17, api.Caller{Node: "other", User: "caller"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("cross-caller chunk err=%v", err)
	}
	first, err := s.GetAuditExportChunk(ctx, task.ID, created.ID, 0, 17, caller)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "later-agent", Host: "test-host", Session: "later-session", Runtime: "codex"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,replaces_agent_id,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, agent.ID, agent.RunID, task.ID, item.ID, item.Revision, task.ID, source.Seq, source.Seq, nil, strings.Repeat("a", 64), []byte(`{"private":"must-not-export"}`), ts(now)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "after snapshot", RequestID: "post-after"}, caller); err != nil {
		t.Fatal(err)
	}
	updatedTitle := "later item title"
	if _, err = s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &updatedTitle}, caller); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CorrectMessageAudit(ctx, task.ID, work.Seq, api.CorrectMessageAuditRequest{RequestID: "later-correction", ExpectedRevision: 1, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditIntake}, Reason: "later correction", Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}}, caller); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CloseTask(ctx, task.ID, caller); err != nil {
		t.Fatal(err)
	}
	content := append([]byte(nil), first.Data...)
	for offset := first.NextOffset; ; {
		chunk, chunkErr := s.GetAuditExportChunk(ctx, task.ID, created.ID, offset, 17, caller)
		if chunkErr != nil {
			t.Fatal(chunkErr)
		}
		if chunk.Offset != offset || chunk.NextOffset != offset+int64(len(chunk.Data)) {
			t.Fatalf("bad boundary: %+v", chunk)
		}
		content = append(content, chunk.Data...)
		offset = chunk.NextOffset
		if chunk.Complete {
			break
		}
	}
	h := sha256.Sum256(content)
	if got := hex.EncodeToString(h[:]); got != created.SHA256 {
		t.Fatalf("digest %s want %s", got, created.SHA256)
	}
	var document struct {
		FormatVersion int                         `json:"formatVersion"`
		Streams       map[string][]map[string]any `json:"streams"`
	}
	if err = json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	if document.FormatVersion != 2 || len(document.Streams["messages"]) != 2 || document.Streams["messages"][0]["text"] != "before snapshot" || document.Streams["task"][0]["status"] != "open" || document.Streams["workItems"][0]["title"] != "frozen item" || len(document.Streams["messageAuditEvents"]) != 1 || len(document.Streams["agents"]) != 0 || len(document.Streams["agentWorkItemBindings"]) != 0 || strings.Contains(string(content), "must-not-export") {
		t.Fatalf("snapshot changed: %#v", document.Streams["messages"])
	}
	replay, err := s.CreateAuditExport(ctx, task.ID, req, caller)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replay || replay.ID != created.ID || replay.SHA256 != created.SHA256 {
		t.Fatalf("bad replay: %+v", replay)
	}
	if _, err = s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: req.RequestID, FormatVersion: 3}, caller); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed intent err=%v", err)
	}
	if _, err = s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "closed-snapshot", FormatVersion: 2}, caller); err != nil {
		t.Fatalf("closed export: %v", err)
	}
	now = now.Add(8 * 24 * time.Hour)
	if _, err = s.GetAuditExportChunk(ctx, task.ID, created.ID, 0, 17, caller); !errors.Is(err, api.ErrExpired) {
		t.Fatalf("expired chunk err=%v", err)
	}
	expired, err := s.CreateAuditExport(ctx, task.ID, req, caller)
	if err != nil {
		t.Fatal(err)
	}
	if expired.ID != created.ID || expired.Available || !expired.Replay {
		t.Fatalf("expired replay regenerated: %+v", expired)
	}
}

func TestAuditExportV3AddsFrozenQueueStreamsWithoutChangingV2Coverage(t *testing.T) {
	s, ctx, caller := workItemStore(t)
	now := time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	source, _ := workItemProject(t, s, ctx, caller, "v3 source", "sourcelead")
	target, _ := workItemProject(t, s, ctx, caller, "v3 target", "targetlead")
	item := createWorkItem(t, s, ctx, caller, source, "v3-queue-item")
	sent, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "v3-send"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := s.CreateAuditExport(ctx, target.ID, api.CreateAuditExportRequest{RequestID: "queue-v2", FormatVersion: 2}, caller)
	if err != nil {
		t.Fatal(err)
	}
	v3, err := s.CreateAuditExport(ctx, target.ID, api.CreateAuditExportRequest{RequestID: "queue-v3", FormatVersion: 3}, caller)
	if err != nil {
		t.Fatal(err)
	}
	read := func(meta api.AuditExport) []byte {
		t.Helper()
		var content []byte
		for offset := int64(0); ; {
			chunk, chunkErr := s.GetAuditExportChunk(ctx, target.ID, meta.ID, offset, 31, caller)
			if chunkErr != nil {
				t.Fatal(chunkErr)
			}
			content = append(content, chunk.Data...)
			offset = chunk.NextOffset
			if chunk.Complete {
				return content
			}
		}
	}
	v2Bytes, v3Bytes := read(v2), read(v3)
	var v2Document, v3Document struct {
		FormatVersion int                         `json:"formatVersion"`
		Streams       map[string][]map[string]any `json:"streams"`
		Cutoffs       map[string]any              `json:"streamCutoffs"`
	}
	if err = json.Unmarshal(v2Bytes, &v2Document); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(v3Bytes, &v3Document); err != nil {
		t.Fatal(err)
	}
	if v2Document.FormatVersion != 2 || v2Document.Streams["queueEntries"] != nil || v2Document.Cutoffs["queueEvents"] != nil {
		t.Fatalf("v2 claimed Queue coverage: streams=%v cutoffs=%v", v2Document.Streams["queueEntries"], v2Document.Cutoffs)
	}
	if v3Document.FormatVersion != 3 || len(v3Document.Streams["queueEntries"]) != 1 || len(v3Document.Streams["queueCycles"]) != 1 || len(v3Document.Streams["queueEvents"]) != 1 || len(v3Document.Streams["queueDispatchLinks"]) != 1 || len(v3Document.Streams["queueNotifications"]) != 1 || v3Document.Cutoffs["queueEvents"] == nil {
		t.Fatalf("v3 Queue coverage incomplete: streams=%v cutoffs=%v", v3Document.Streams, v3Document.Cutoffs)
	}
	if _, err = s.QueueAction(ctx, target.ID, sent.Queue.Entry.ID, api.QueueActionRequest{Operation: "priority", RequestID: "after-v3", ExpectedRevision: sent.Queue.Entry.Revision, Cycle: 1, Priority: "urgent"}, caller); err != nil {
		t.Fatal(err)
	}
	if later := read(v3); !json.Valid(later) || string(later) != string(v3Bytes) || len(later) != int(v3.ByteCount) {
		t.Fatal("frozen v3 Queue export changed after a later Queue mutation")
	}
	digest := sha256.Sum256(v3Bytes)
	if hex.EncodeToString(digest[:]) != v3.SHA256 {
		t.Fatal("v3 Queue export digest mismatch")
	}
}

// TestAuditExportAllocationIntentStreamOnlyInV3 addresses independent review
// #3003/#3010's audit-export gap: agent_allocation_intents (and its
// review-#2916 expected-run/launcher/author columns) postdates legacy
// format 2. Format 2's schema must remain byte-for-byte unchanged for any
// existing consumer; the allocationIntent stream is included only in format
// 3, exactly like the Queue streams already proven this way above.
func TestAuditExportAllocationIntentStreamOnlyInV3(t *testing.T) {
	s, ctx, by := workItemStore(t)
	now := time.Date(2026, 9, 12, 14, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	task, lead := workItemProject(t, s, ctx, by, "intent export", "lead")
	item := createWorkItem(t, s, ctx, by, task, "intent-export-item")
	order := contextLinkedMessage(t, s, task, item, "intent export order", "intent-export-order", nil)
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	bundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order))
	digestBytes := sha256.Sum256(bundle)
	digest := hex.EncodeToString(digestBytes[:])
	if _, err := s.CreateAllocationIntent(ctx, task.ID, api.CreateAllocationIntentRequest{
		AgentID: api.NewID("agt"), TargetTaskID: task.ID, ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef,
		ContextDigest: digest, TeamRole: api.TeamRoleExtra, AuthorAgentID: lead.ID, AuthorRunID: lead.RunID, ExpectedRunID: api.NewID("run"),
	}, by); err != nil {
		t.Fatal(err)
	}
	v2, err := s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "intent-v2", FormatVersion: 2}, by)
	if err != nil {
		t.Fatal(err)
	}
	v3, err := s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "intent-v3", FormatVersion: 3}, by)
	if err != nil {
		t.Fatal(err)
	}
	read := func(meta api.AuditExport) []byte {
		t.Helper()
		var content []byte
		for offset := int64(0); ; {
			chunk, chunkErr := s.GetAuditExportChunk(ctx, task.ID, meta.ID, offset, 31, by)
			if chunkErr != nil {
				t.Fatal(chunkErr)
			}
			content = append(content, chunk.Data...)
			offset = chunk.NextOffset
			if chunk.Complete {
				return content
			}
		}
	}
	var v2Document, v3Document struct {
		FormatVersion int                         `json:"formatVersion"`
		Streams       map[string][]map[string]any `json:"streams"`
	}
	if err = json.Unmarshal(read(v2), &v2Document); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(read(v3), &v3Document); err != nil {
		t.Fatal(err)
	}
	if v2Document.FormatVersion != 2 || v2Document.Streams["agentAllocationIntents"] != nil {
		t.Fatalf("v2 claimed allocation-intent coverage: streams=%v", v2Document.Streams["agentAllocationIntents"])
	}
	if v3Document.FormatVersion != 3 || len(v3Document.Streams["agentAllocationIntents"]) != 1 ||
		v3Document.Streams["agentAllocationIntents"][0]["team_role"] != api.TeamRoleExtra ||
		v3Document.Streams["agentAllocationIntents"][0]["context_digest"] != digest {
		t.Fatalf("v3 allocation-intent coverage incomplete: streams=%v", v3Document.Streams["agentAllocationIntents"])
	}
}

func TestAuditExportContentAndProjectByteLimitsFailBeforeReceipt(t *testing.T) {
	ctx := context.Background()
	caller := api.Caller{Node: "n", User: "u"}
	t.Run("canonical content", func(t *testing.T) {
		s, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "content bound"}, caller)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO events(task_id,kind,text,data,by_node,by_user,created_at) VALUES(?,?,?,?,?,?,?)`, task.ID, "synthetic", "", strings.Repeat("x", api.MaxAuditExportBytes), caller.Node, caller.User, ts(s.now())); err != nil {
			t.Fatal(err)
		}
		if _, err = s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "too-large", FormatVersion: 2}, caller); !errors.Is(err, api.ErrLimit) {
			t.Fatalf("content limit err=%v", err)
		}
		var exports, receipts int
		if err = s.db.QueryRow(`SELECT (SELECT count(*) FROM audit_exports),(SELECT count(*) FROM audit_export_receipts)`).Scan(&exports, &receipts); err != nil || exports != 0 || receipts != 0 {
			t.Fatalf("partial limit commit exports=%d receipts=%d err=%v", exports, receipts, err)
		}
	})
	t.Run("many row canonical content", func(t *testing.T) {
		s, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "many row bound"}, caller)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		payload := strings.Repeat("m", 60*1024)
		for index := 0; index < 560; index++ {
			if _, err = tx.ExecContext(ctx, `INSERT INTO events(task_id,kind,text,data,by_node,by_user,created_at) VALUES(?,?,?,?,?,?,?)`, task.ID, "synthetic", "", payload, caller.Node, caller.User, ts(s.now())); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err = s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "many-too-large", FormatVersion: 2}, caller); !errors.Is(err, api.ErrLimit) {
			t.Fatalf("many-row content limit err=%v", err)
		}
		var exports, receipts int
		if err = s.db.QueryRow(`SELECT (SELECT count(*) FROM audit_exports),(SELECT count(*) FROM audit_export_receipts)`).Scan(&exports, &receipts); err != nil || exports != 0 || receipts != 0 {
			t.Fatalf("many-row partial commit exports=%d receipts=%d err=%v", exports, receipts, err)
		}
	})
	t.Run("project ready bytes", func(t *testing.T) {
		s, err := Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		now := s.now()
		task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "project bound"}, caller)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO audit_exports(id,task_id,format_version,request_hash,by_node,by_user,created_at,expires_at,byte_count,sha256,cutoffs,content) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, "aex_0000000000000001", task.ID, 2, "seed", caller.Node, caller.User, ts(now), ts(now.Add(time.Hour)), api.MaxReadyAuditExportBytes-1, strings.Repeat("0", 64), "{}", []byte{1}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "over-project-bytes", FormatVersion: 2}, caller); !errors.Is(err, api.ErrLimit) {
			t.Fatalf("project byte limit err=%v", err)
		}
		var receipts int
		if err = s.db.QueryRow(`SELECT count(*) FROM audit_export_receipts`).Scan(&receipts); err != nil || receipts != 0 {
			t.Fatalf("partial project-limit receipt count=%d err=%v", receipts, err)
		}
	})
}

func TestAuditExportReadyCountLimit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	caller := api.Caller{Node: "n", User: "u"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "limits"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < api.MaxReadyAuditExports; i++ {
		if _, err = s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "export-" + string(rune('a'+i)), FormatVersion: 2}, caller); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.CreateAuditExport(ctx, task.ID, api.CreateAuditExportRequest{RequestID: "export-over", FormatVersion: 2}, caller); !errors.Is(err, api.ErrLimit) {
		t.Fatalf("limit err=%v", err)
	}
}
