package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func messageAuditRequest(item api.WorkItem, target api.Agent, key string) api.PostMessageRequest {
	return api.PostMessageRequest{
		Text:      "Store the audited result",
		To:        target.ID,
		RequestID: key,
		WorkItems: []api.MessageWorkItem{{
			ItemTaskID:   item.TaskID,
			ItemID:       item.ID,
			ItemRevision: item.Revision,
			Relationship: "primary",
		}},
	}
}

func TestMessageAuditFailuresRollbackContentLinkEventsResumeAndReceipt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger string
	}{
		{"link", `CREATE TRIGGER fail_audit_link BEFORE INSERT ON message_work_item_links BEGIN SELECT RAISE(ABORT,'link failed'); END`},
		{"message event", `CREATE TRIGGER fail_audit_message_event BEFORE INSERT ON events WHEN NEW.kind='message' BEGIN SELECT RAISE(ABORT,'event failed'); END`},
		{"resume event", `CREATE TRIGGER fail_audit_resume_event BEFORE INSERT ON events WHEN NEW.kind='resumed' BEGIN SELECT RAISE(ABORT,'resume failed'); END`},
		{"receipt", `CREATE TRIGGER fail_audit_receipt BEFORE INSERT ON message_post_requests BEGIN SELECT RAISE(ABORT,'receipt failed'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ctx, by := workItemStore(t)
			project, _ := workItemProject(t, s, ctx, by, "Audit rollback", "lead")
			item := createWorkItem(t, s, ctx, by, project, "rollback-item")
			target, err := s.AddAgent(ctx, project.ID, api.AddAgentRequest{Name: "target", Host: "host", Session: "target", Runtime: "codex"}, by)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.PostEvent(ctx, project.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: target.ID, RunID: target.RunID}, by); err != nil {
				t.Fatal(err)
			}
			retired := api.AgentRetired
			if _, err = s.UpdateAgent(ctx, target.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
				t.Fatal(err)
			}
			var beforeMessages, beforeLinks, beforeEvents, beforeReceipts int
			if err = s.db.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM messages),(SELECT count(*) FROM message_work_item_links),
(SELECT count(*) FROM events),(SELECT count(*) FROM message_post_requests)`).Scan(
				&beforeMessages, &beforeLinks, &beforeEvents, &beforeReceipts,
			); err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.ExecContext(ctx, tc.trigger); err != nil {
				t.Fatal(err)
			}
			request := messageAuditRequest(item, target, "rollback-post")
			if _, err = s.PostMessage(ctx, project.ID, request, by); err == nil {
				t.Fatal("injected failure unexpectedly committed")
			}
			var messages, links, events, receipts int
			if err = s.db.QueryRowContext(ctx, `SELECT
(SELECT count(*) FROM messages),(SELECT count(*) FROM message_work_item_links),
(SELECT count(*) FROM events),(SELECT count(*) FROM message_post_requests)`).Scan(
				&messages, &links, &events, &receipts,
			); err != nil {
				t.Fatal(err)
			}
			if messages != beforeMessages || links != beforeLinks || events != beforeEvents || receipts != beforeReceipts {
				t.Fatalf("partial write: messages=%d/%d links=%d/%d events=%d/%d receipts=%d/%d",
					messages, beforeMessages, links, beforeLinks, events, beforeEvents, receipts, beforeReceipts)
			}
			persisted, err := s.GetAgent(ctx, target.ID)
			if err != nil || persisted.Status != api.AgentRetired || persisted.RunID != target.RunID {
				t.Fatalf("failed post changed recipient lifecycle: %+v %v", persisted, err)
			}
			if _, err = s.GetMessagePostReceipt(ctx, project.ID, request.RequestID, "", by); !errors.Is(err, api.ErrNotFound) {
				t.Fatalf("failed post left recoverable receipt: %v", err)
			}
		})
	}
}

func TestMessageAuditMigrationPreservesLegacyRowsAndAddsContext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pre-a1.sqlite")
	by := api.Caller{Node: "legacy-node", User: "legacy-user"}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := workItemProject(t, s, ctx, by, "Legacy audit migration", "lead")
	retiredAgent, err := s.AddAgent(ctx, project.ID, api.AddAgentRequest{Name: "retired", Host: "host", Session: "retired", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	retired := api.AgentRetired
	if _, err = s.UpdateAgent(ctx, retiredAgent.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE agents SET cleanup_done=1,cleanup_error='legacy cleanup receipt' WHERE id=?`, retiredAgent.ID); err != nil {
		t.Fatal(err)
	}
	profileKey := strings.Repeat("a", 64)
	profile, err := s.CreateProfile(ctx, "legacy-profile", profileKey, profileFixture)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.PostMessage(ctx, project.ID, api.PostMessageRequest{Text: "Original legacy message"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, project.ID, api.CreateWorkItemRequest{
		Kind: "bug", Title: "Preserve legacy rows", SourceMessageSeq: legacy.Seq, RequestID: "legacy-item",
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE message_post_requests; DROP TABLE message_work_item_links;
DROP INDEX messages_task_seq_unique; DROP INDEX work_items_task_id_unique;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	messages, err := s.ListMessages(ctx, project.ID, 0, "", 10)
	if err != nil || len(messages) != 1 || messages[0].Seq != legacy.Seq || messages[0].Text != legacy.Text || messages[0].From != legacy.From || messages[0].PostReceipt != nil || messages[0].WorkItems != nil {
		t.Fatalf("legacy message changed during migration: %+v %v", messages, err)
	}
	persistedItem, err := s.GetWorkItem(ctx, project.ID, item.ID)
	if err != nil || persistedItem.Revision != item.Revision || persistedItem.SourceMessageSeq != legacy.Seq {
		t.Fatalf("work-item history changed during migration: %+v %v", persistedItem, err)
	}
	persistedTask, err := s.GetTask(ctx, project.ID)
	if err != nil || persistedTask.Status != api.TaskOpen {
		t.Fatalf("task status changed during migration: %+v %v", persistedTask, err)
	}
	persistedAgent, err := s.GetAgent(ctx, retiredAgent.ID)
	if err != nil || persistedAgent.Status != api.AgentRetired || !persistedAgent.CleanupDone || persistedAgent.CleanupError != "legacy cleanup receipt" {
		t.Fatalf("agent retirement/cleanup changed during migration: %+v %v", persistedAgent, err)
	}
	persistedProfile, err := s.ReadProfile(ctx, profile.Username, profileKey)
	if err != nil || persistedProfile.Revision != profile.Revision || string(persistedProfile.Envelope) != string(profile.Envelope) {
		t.Fatalf("profile changed during migration: %+v %v", persistedProfile, err)
	}
	linked, err := s.PostMessage(ctx, project.ID, api.PostMessageRequest{
		Text: "First contextual message", RequestID: "after-migration",
		WorkItems: []api.MessageWorkItem{{ItemTaskID: project.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
	}, by)
	if err != nil || linked.PostReceipt == nil || len(linked.WorkItems) != 1 {
		t.Fatalf("migrated tables unusable: %+v %v", linked, err)
	}
	var violations int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign-key integrity: %d %v", violations, err)
	}
}
