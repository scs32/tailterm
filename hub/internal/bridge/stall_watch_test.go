package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/store"
)

// stallWatchNotices runs the hub's stall watch sweep over a synthetic project
// in a temporary database and returns the notices it posted, so the render
// test reads the hub's real refs and text.
func stallWatchNotices(t *testing.T, start time.Time, quiet time.Duration, team, owes bool) (api.Task, api.Agent, []api.Message) {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "test", User: "owner"}
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := start
	st.SetClockForTest(func() time.Time { return clock })
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Stall watch render"}, by)
	if err != nil {
		t.Fatal(err)
	}
	out, err := st.RegisterOwnerHelper(ctx, task.ID, api.RegisterOwnerHelperRequest{Host: "owner-host", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: "stall-helper"}, by)
	if err != nil {
		t.Fatal(err)
	}
	helper := *out.Agent
	if owes {
		if _, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{To: helper.ID, Text: "Owner asks the helper for a status"}, by); err != nil {
			t.Fatal(err)
		}
	}
	if team {
		member, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead-a", Host: "h", Session: "lead-a", Runtime: "codex"}, by)
		if err != nil {
			t.Fatal(err)
		}
		item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Quiet team", RequestID: "stall-item"}, by)
		if err != nil {
			t.Fatal(err)
		}
		// A running queue entry has no short public path from this package;
		// the rows go into this test's own database file.
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		at := start.Format(time.RFC3339Nano)
		for _, stmt := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,created_at,updated_at,admitted_at) VALUES('tqe_stall',?,?,1,1,'planned',1,'running',?,?,?)`, []any{task.ID, item.ID, at, at, at}},
			{`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,1,0,'digest','{}',?)`,
				[]any{member.ID, member.RunID, task.ID, item.ID, task.ID, at}},
		} {
			if _, err := db.Exec(stmt.query, stmt.args...); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	clock = start.Add(quiet)
	if !owes {
		// The helper's session is still there: its wrapper heartbeats.
		if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: helper.ID, RunID: helper.RunID, Kind: api.EventHeartbeat}, by); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := st.StallWatchSweep(ctx, clock)
	if err != nil {
		t.Fatal(err)
	}
	var notices []api.Message
	msgs, err := st.ListMessages(ctx, task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		for _, m := range msgs {
			if m.Seq == s.MessageSeq {
				notices = append(notices, m)
			}
		}
	}
	if len(notices) != len(steps) {
		t.Fatalf("%d notices for %d steps", len(notices), len(steps))
	}
	return task, helper, notices
}

// a9 (wi_fa5f10477f93c57d): how the bridge mirrors the stall watch notices.
func TestStallWatchNoticesRender(t *testing.T) {
	noon := time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC)        // 12:00 Pacific
	lateEvening := time.Date(2026, 10, 13, 5, 40, 0, 0, time.UTC) // 22:40 Pacific
	b := &Bridge{cfg: Config{TailOSURL: "https://tailos.example"}, ownerList: []string{"111", "222"}}
	payload := func(t *testing.T, rows []OutboxRow) outboxPayload {
		t.Helper()
		if len(rows) != 1 {
			t.Fatalf("%d rows, want one", len(rows))
		}
		var p outboxPayload
		if err := json.Unmarshal([]byte(rows[0].Payload), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	labels := func(p outboxPayload) (out []string) {
		var walk func(cs []discord.Component)
		walk = func(cs []discord.Component) {
			for _, c := range cs {
				if c.Label != "" {
					out = append(out, c.Label+"|"+c.CustomID)
				}
				walk(c.Components)
			}
		}
		walk(p.Message.Components)
		return out
	}
	t.Run("owner ping", func(t *testing.T) {
		for _, team := range []bool{false, true} {
			task, helper, notices := stallWatchNotices(t, noon, 30*time.Minute, team, true)
			want := 1
			if team {
				want = 2 // the helper is quiet too, so the team notice is the owner's
			}
			if len(notices) != want {
				t.Fatalf("team=%v: %d notices, want %d", team, len(notices), want)
			}
			for _, m := range notices {
				if m.Envelope.Refs["escalation"] != "owner" || m.To != "" {
					t.Fatalf("notice to %q refs %+v", m.To, m.Envelope.Refs)
				}
				rows := b.renderMessage(task, roster{helper.ID: helper}, m)
				p := payload(t, rows)
				if rows[0].Kind != kindEscalation || strings.Join(p.MentionUser, ",") != "111,222" || strings.Count(p.Message.Content, "<@") != 2 ||
					!strings.HasPrefix(p.Message.Content, "<@111> <@222> ") || !strings.Contains(p.Message.Content, m.Envelope.Subject) {
					t.Fatalf("owner ping row %s mentions %v content %q", rows[0].Kind, p.MentionUser, p.Message.Content)
				}
				for _, label := range labels(p) {
					for _, banned := range []string{"Nudge", "Extend", "Reassign", "nudge:", "extend30:", "reassign:"} {
						if strings.Contains(label, banned) {
							t.Fatalf("owner ping has the button %q", label)
						}
					}
				}
				// The observation, the unknown cause and the record owed come
				// first, inside what the ping shows.
				for _, part := range []string{"has been quiet for 30 min", "The hub does not know why it stopped.", "Before resuming, record why it stopped"} {
					if !strings.Contains(p.Message.Content, part) {
						t.Fatalf("ping lacks %q: %q", part, p.Message.Content)
					}
				}
			}
		}
	})
	t.Run("night line", func(t *testing.T) {
		task, helper, notices := stallWatchNotices(t, lateEvening, 20*time.Minute, false, true)
		if len(notices) != 1 || notices[0].Envelope.Refs["escalation"] != "lead" {
			t.Fatalf("night notices %+v", notices)
		}
		rows := b.renderMessage(task, roster{helper.ID: helper}, notices[0])
		p := payload(t, rows)
		raw := rows[0].Payload
		if rows[0].Kind != kindLine || len(p.MentionUser) != 0 || strings.Contains(raw, "<@") || len(labels(p)) != 0 {
			t.Fatalf("night line row %s mentions %v payload %s", rows[0].Kind, p.MentionUser, raw)
		}
		if !strings.Contains(p.Message.Content, notices[0].Envelope.Subject) || len(p.Message.Embeds) != 1 || !strings.Contains(p.Message.Embeds[0].Description, "The owner is pinged at 07:00 Pacific if this is still open.") {
			t.Fatalf("night line content %q embeds %+v", p.Message.Content, p.Message.Embeds)
		}
	})
	t.Run("directed helper notice", func(t *testing.T) {
		task, helper, notices := stallWatchNotices(t, noon, 30*time.Minute, true, false)
		if len(notices) != 1 || notices[0].To != helper.ID || notices[0].Envelope.Refs["stallWatch"] != "team" || notices[0].Envelope.Refs["escalation"] != "" {
			t.Fatalf("directed notices %+v", notices)
		}
		if rows := b.renderMessage(task, roster{helper.ID: helper}, notices[0]); len(rows) != 0 {
			t.Fatalf("the directed helper notice rendered %d rows", len(rows))
		}
	})
}
