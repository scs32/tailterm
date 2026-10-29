package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type interventionFixture struct {
	s       *Store
	ctx     context.Context
	by      api.Caller
	task    api.Task
	item    api.WorkItem
	product api.WorkItem
}

// newInterventionFixture builds a shadow project with one concerned item and
// a separate product home project holding the item expected to remove it.
func newInterventionFixture(t *testing.T) interventionFixture {
	t.Helper()
	s, ctx, by := workItemStore(t)
	task, _ := workItemProject(t, s, ctx, by, "Synthetic shadow", "")
	home, _ := workItemProject(t, s, ctx, by, "Synthetic product home", "")
	return interventionFixture{s: s, ctx: ctx, by: by, task: task,
		item: createWorkItem(t, s, ctx, by, task, "concerned-item"), product: createWorkItem(t, s, ctx, by, home, "product-item")}
}

func (f interventionFixture) request(key, kind string) api.CreateInterventionRequest {
	return api.CreateInterventionRequest{Kind: kind, ItemID: f.item.ID, ProductItemID: f.product.ID, Text: "Released the candidate by hand", RequestID: key}
}

func countInterventionRows(t *testing.T, s *Store) [6]int {
	t.Helper()
	var out [6]int
	if err := s.db.QueryRow(`SELECT
(SELECT count(*) FROM messages),(SELECT count(*) FROM owner_interventions),(SELECT count(*) FROM events),
(SELECT count(*) FROM message_post_requests),(SELECT count(*) FROM message_work_item_links),(SELECT count(*) FROM message_checks)`).Scan(
		&out[0], &out[1], &out[2], &out[3], &out[4], &out[5]); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInterventionRecordsOwnerMessageLinkedToCurrentItemRevision(t *testing.T) {
	f := newInterventionFixture(t)
	title := "Advanced before the intervention"
	item, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Title: &title}, f.by)
	if err != nil || item.Revision != 2 {
		t.Fatalf("advance item: %+v %v", item, err)
	}
	req := f.request("intervention-roundtrip", "nudge")
	created, err := f.s.CreateIntervention(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	want := api.Intervention{Kind: "nudge", ItemTaskID: f.task.ID, ItemID: f.item.ID, ProductTaskID: f.product.TaskID, ProductItemID: f.product.ID, Actor: "unknown"}
	if created.Intervention == nil || *created.Intervention != want || f.product.TaskID == f.task.ID {
		t.Fatalf("intervention record: %+v", created.Intervention)
	}
	if created.From != (api.Sender{Node: f.by.Node, User: f.by.User}) || created.To != "" || created.Envelope != nil || created.Text != api.FormatIntervention(req) {
		t.Fatalf("intervention message: %+v", created)
	}
	wantLinks := []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: 2, Relationship: "primary"}}
	if !reflect.DeepEqual(created.WorkItems, wantLinks) || created.PostReceipt == nil || created.PostReceipt.RequestID != req.RequestID {
		t.Fatalf("intervention links: %+v receipt %+v", created.WorkItems, created.PostReceipt)
	}
	messages, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 0)
	if err != nil || len(messages) != 1 || messages[0].Intervention == nil || *messages[0].Intervention != want || !reflect.DeepEqual(messages[0].WorkItems, wantLinks) {
		t.Fatalf("board read: %+v %v", messages, err)
	}

	before := countInterventionRows(t, f.s)
	replayed, err := f.s.CreateIntervention(f.ctx, f.task.ID, req, f.by)
	if err != nil || replayed.Seq != created.Seq || !reflect.DeepEqual(replayed.Intervention, created.Intervention) {
		t.Fatalf("replay: %+v %v", replayed, err)
	}
	changed := req
	changed.Text = "A different intervention"
	if _, err = f.s.CreateIntervention(f.ctx, f.task.ID, changed, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed payload err=%v", err)
	}
	if after := countInterventionRows(t, f.s); after != before {
		t.Fatalf("retries wrote rows: before=%v after=%v", before, after)
	}

	// The record shows in the concerned item's message history.
	history, err := f.s.ListWorkItemMessages(f.ctx, f.task.ID, f.item.ID, 0, 0, api.MaxWorkItemHistoryPage)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, link := range history.Links {
		found = found || (link.Message.Seq == created.Seq && link.Message.Intervention != nil && link.ItemRevision == 2)
	}
	if !found {
		t.Fatalf("item history lacks intervention: %+v", history.Links)
	}

	// Interventions are human posts, so they never count toward typed-agent adoption.
	checks, err := f.s.ListMessageChecks(f.ctx, f.task.ID, 0, 0)
	if err != nil || len(checks) != 1 || checks[0].Seq != created.Seq || checks[0].Form != api.CheckFormHuman || checks[0].AgentID != "" {
		t.Fatalf("message check: %+v %v", checks, err)
	}
}

func TestInterventionAcceptsEveryKindAndOptionalProductItem(t *testing.T) {
	f := newInterventionFixture(t)
	for _, kind := range api.InterventionKinds {
		m, err := f.s.CreateIntervention(f.ctx, f.task.ID, f.request("kind-"+kind, kind), f.by)
		if err != nil || m.Intervention == nil || m.Intervention.Kind != kind {
			t.Fatalf("kind %s: %+v %v", kind, m, err)
		}
	}
	if len(api.InterventionKinds) != 8 {
		t.Fatalf("kinds = %v", api.InterventionKinds)
	}
	req := f.request("no-product", "cleanup")
	req.ProductItemID = ""
	m, err := f.s.CreateIntervention(f.ctx, f.task.ID, req, f.by)
	if err != nil || m.Intervention.ProductItemID != "" || m.Intervention.ProductTaskID != "" || strings.Contains(m.Text, "product fix") {
		t.Fatalf("unlinked intervention: %+v %v", m, err)
	}
	// A done item may still be the one the owner intervened on.
	done := "done"
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.CreateIntervention(f.ctx, f.task.ID, f.request("done-item", "release"), f.by); err != nil {
		t.Fatalf("done item: %v", err)
	}
}

func TestInterventionRejectionsWriteNothing(t *testing.T) {
	f := newInterventionFixture(t)
	other, _ := workItemProject(t, f.s, f.ctx, f.by, "Synthetic other", "")
	otherItem := createWorkItem(t, f.s, f.ctx, f.by, other, "other-item")
	cases := []struct {
		name   string
		mutate func(*api.CreateInterventionRequest)
		want   error
	}{
		{"unknown kind", func(r *api.CreateInterventionRequest) { r.Kind = "foo" }, api.ErrInvalid},
		{"blank text", func(r *api.CreateInterventionRequest) { r.Text = " \n\t" }, api.ErrInvalid},
		{"control text", func(r *api.CreateInterventionRequest) { r.Text = "bad\x00text" }, api.ErrInvalid},
		{"malformed item", func(r *api.CreateInterventionRequest) { r.ItemID = "tsk_0000000000000001" }, api.ErrInvalid},
		{"unknown item", func(r *api.CreateInterventionRequest) { r.ItemID = "wi_0000000000000001" }, api.ErrNotFound},
		{"other project item", func(r *api.CreateInterventionRequest) { r.ItemID = otherItem.ID }, api.ErrNotFound},
		{"unknown product item", func(r *api.CreateInterventionRequest) { r.ProductItemID = "wi_0000000000000001" }, api.ErrNotFound},
		{"missing request id", func(r *api.CreateInterventionRequest) { r.RequestID = "" }, api.ErrInvalid},
		{"agent identity", func(r *api.CreateInterventionRequest) { r.AgentID = "agt_0000000000000001" }, ErrInterventionForbidden},
	}
	before := countInterventionRows(t, f.s)
	for _, tc := range cases {
		req := f.request("reject-"+strings.ReplaceAll(tc.name, " ", "-"), "nudge")
		tc.mutate(&req)
		if _, err := f.s.CreateIntervention(f.ctx, f.task.ID, req, f.by); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err=%v want %v", tc.name, err, tc.want)
		}
	}
	if after := countInterventionRows(t, f.s); after != before {
		t.Fatalf("rejections wrote rows: before=%v after=%v", before, after)
	}
	if _, err := f.s.CloseTask(f.ctx, f.task.ID, f.by); err != nil {
		t.Fatal(err)
	}
	before = countInterventionRows(t, f.s)
	if _, err := f.s.CreateIntervention(f.ctx, f.task.ID, f.request("closed-project", "nudge"), f.by); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("closed project err=%v", err)
	}
	if after := countInterventionRows(t, f.s); after != before {
		t.Fatalf("closed project wrote rows: before=%v after=%v", before, after)
	}
}

func TestInterventionFailuresRollbackAtomically(t *testing.T) {
	for _, tc := range []struct{ name, trigger string }{
		{"message event", `CREATE TRIGGER fail_intervention_event BEFORE INSERT ON events WHEN NEW.kind='message' BEGIN SELECT RAISE(ABORT,'event failed'); END`},
		{"intervention record", `CREATE TRIGGER fail_intervention_record BEFORE INSERT ON owner_interventions BEGIN SELECT RAISE(ABORT,'record failed'); END`},
		{"receipt", `CREATE TRIGGER fail_intervention_receipt BEFORE INSERT ON message_post_requests BEGIN SELECT RAISE(ABORT,'receipt failed'); END`},
		{"item link", `CREATE TRIGGER fail_intervention_link BEFORE INSERT ON message_work_item_links BEGIN SELECT RAISE(ABORT,'link failed'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInterventionFixture(t)
			before := countInterventionRows(t, f.s)
			if _, err := f.s.db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.CreateIntervention(f.ctx, f.task.ID, f.request("rollback", "gate-fix"), f.by); err == nil {
				t.Fatal("injected failure committed")
			}
			if after := countInterventionRows(t, f.s); after != before {
				t.Fatalf("partial write: before=%v after=%v", before, after)
			}
		})
	}
}

func TestInterventionsAreImmutableAndExported(t *testing.T) {
	f := newInterventionFixture(t)
	created, err := f.s.CreateIntervention(f.ctx, f.task.ID, f.request("immutable", "diagnosis"), f.by)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE owner_interventions SET kind='other' WHERE message_seq=?`,
		`UPDATE owner_interventions SET product_item_id='' WHERE message_seq=?`,
		`UPDATE owner_interventions SET actor='owner' WHERE message_seq=?`,
		`DELETE FROM owner_interventions WHERE message_seq=?`,
	} {
		if _, err := f.s.db.Exec(statement, created.Seq); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("%s: err=%v", statement, err)
		}
	}
	messages, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 0)
	if err != nil || len(messages) != 1 || *messages[0].Intervention != *created.Intervention {
		t.Fatalf("record changed: %+v %v", messages, err)
	}

	export, err := f.s.CreateAuditExport(f.ctx, f.task.ID, api.CreateAuditExportRequest{RequestID: "intervention-export", FormatVersion: 2}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	var content []byte
	for offset := int64(0); ; {
		chunk, err := f.s.GetAuditExportChunk(f.ctx, f.task.ID, export.ID, offset, api.MaxAuditExportChunkBytes, f.by)
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, chunk.Data...)
		offset = chunk.NextOffset
		if chunk.Complete {
			break
		}
	}
	var document struct {
		Streams map[string][]map[string]any `json:"streams"`
	}
	if err = json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	rows := document.Streams["ownerInterventions"]
	if len(rows) != 1 || rows[0]["kind"] != "diagnosis" || rows[0]["item_id"] != f.item.ID || rows[0]["product_item_id"] != f.product.ID || rows[0]["message_seq"] != float64(created.Seq) || rows[0]["actor"] != "unknown" {
		t.Fatalf("export rows: %#v", rows)
	}
}

func TestInterventionSummaryBucketsLocalDaysAndPages(t *testing.T) {
	f := newInterventionFixture(t)
	clock := time.Date(2026, 9, 28, 6, 30, 0, 0, time.UTC)
	f.s.now = func() time.Time { return clock }
	first, err := f.s.CreateIntervention(f.ctx, f.task.ID, f.request("day-one", "nudge"), f.by)
	if err != nil {
		t.Fatal(err)
	}
	clock = time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	unlinked := f.request("day-two", "release")
	unlinked.ProductItemID = ""
	second, err := f.s.CreateIntervention(f.ctx, f.task.ID, unlinked, f.by)
	if err != nil {
		t.Fatal(err)
	}
	third, err := f.s.CreateIntervention(f.ctx, f.task.ID, f.request("day-two-b", "nudge"), f.by)
	if err != nil {
		t.Fatal(err)
	}
	// Another project's interventions never leak into this summary.
	other, _ := workItemProject(t, f.s, f.ctx, f.by, "Synthetic neighbour", "")
	otherItem := createWorkItem(t, f.s, f.ctx, f.by, other, "neighbour-item")
	if _, err = f.s.CreateIntervention(f.ctx, other.ID, api.CreateInterventionRequest{Kind: "status", ItemID: otherItem.ID, Text: "Elsewhere", RequestID: "neighbour"}, f.by); err != nil {
		t.Fatal(err)
	}

	la, err := f.s.ListInterventions(f.ctx, f.task.ID, 0, 0, "America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// Read at the last record's clock: every row is inside both windows.
	wantLA := api.InterventionSummary{
		TimeZone: "America/Los_Angeles", Total: 3, Linked: 2, ByKind: map[string]int{"nudge": 2, "release": 1}, ByActor: map[string]int{"unknown": 3},
		Windows: []api.InterventionWindow{{Hours: 6, Total: 3, ByActor: map[string]int{"unknown": 3}}, {Hours: 24, Total: 3, ByActor: map[string]int{"unknown": 3}}},
		Days: []api.InterventionDay{
			{Day: "2026-09-28", Total: 2, Linked: 1, ByKind: map[string]int{"nudge": 1, "release": 1}, ByActor: map[string]int{"unknown": 2}},
			{Day: "2026-09-27", Total: 1, Linked: 1, ByKind: map[string]int{"nudge": 1}, ByActor: map[string]int{"unknown": 1}},
		},
		Unlinked: []api.InterventionRef{{Seq: second.Seq, Kind: "release", ItemID: f.item.ID, Actor: "unknown", Day: "2026-09-28"}},
	}
	if !reflect.DeepEqual(la.Summary, wantLA) {
		t.Fatalf("LA summary:\n got %+v\nwant %+v", la.Summary, wantLA)
	}
	if len(la.Interventions) != 3 || la.Interventions[0].Seq != first.Seq || la.Interventions[2].Seq != third.Seq || la.NextAfter != 0 {
		t.Fatalf("LA page: %+v next=%d", la.Interventions, la.NextAfter)
	}

	utc, err := f.s.ListInterventions(f.ctx, f.task.ID, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if utc.Summary.TimeZone != "UTC" || len(utc.Summary.Days) != 1 || utc.Summary.Days[0].Day != "2026-09-28" || utc.Summary.Days[0].Total != 3 || utc.Summary.Days[0].Linked != 2 {
		t.Fatalf("UTC summary: %+v", utc.Summary)
	}

	for _, bad := range []string{"Mars/Olympus", "Local", "../etc/passwd", "America/Los_Angeles\x00"} {
		if _, err = f.s.ListInterventions(f.ctx, f.task.ID, 0, 0, bad); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("tz %q err=%v", bad, err)
		}
	}
	if _, err = f.s.ListInterventions(f.ctx, f.task.ID, 0, api.MaxInterventionPage+1, ""); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("oversized page err=%v", err)
	}

	// Paging returns each record exactly once; the summary stays whole-project.
	var seen []int64
	for after := int64(0); ; {
		page, err := f.s.ListInterventions(f.ctx, f.task.ID, after, 1, "America/Los_Angeles")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(page.Summary, wantLA) {
			t.Fatalf("page summary changed: %+v", page.Summary)
		}
		for _, m := range page.Interventions {
			seen = append(seen, m.Seq)
		}
		if page.NextAfter == 0 {
			break
		}
		after = page.NextAfter
	}
	if !reflect.DeepEqual(seen, []int64{first.Seq, second.Seq, third.Seq}) {
		t.Fatalf("paged seqs = %v", seen)
	}
}

// preActorInterventionRequest is CreateInterventionRequest as it was before
// the actor field, so a receipt stored then can be checked against today's hash.
type preActorInterventionRequest struct {
	Kind          string `json:"kind"`
	ItemID        string `json:"itemId"`
	ProductItemID string `json:"productItemId,omitempty"`
	Text          string `json:"text"`
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId,omitempty"`
}

func interventionReceiptHash(t *testing.T, s *Store, seq int64) string {
	t.Helper()
	var hash string
	if err := s.db.QueryRow(`SELECT payload_hash FROM message_post_requests WHERE message_seq=?`, seq).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestInterventionRecordsActor(t *testing.T) {
	f := newInterventionFixture(t)
	for _, actor := range []string{"owner", "helper", ""} {
		req := f.request("actor-"+actor, "decision-on-behalf")
		req.Actor = actor
		want := actor
		if want == "" {
			want = "unknown"
		}
		created, err := f.s.CreateIntervention(f.ctx, f.task.ID, req, f.by)
		if err != nil || created.Intervention == nil || created.Intervention.Actor != want {
			t.Fatalf("create actor %q: %+v %v", actor, created.Intervention, err)
		}
		if created.Text != api.FormatIntervention(req) {
			t.Fatalf("actor %q changed the Board text: %q", actor, created.Text)
		}
		var stored string
		if err = f.s.db.QueryRow(`SELECT actor FROM owner_interventions WHERE message_seq=?`, created.Seq).Scan(&stored); err != nil || stored != want {
			t.Fatalf("stored actor %q: %q %v", actor, stored, err)
		}
		message, err := loadMessage(f.s.db, f.ctx, f.task.ID, created.Seq)
		if err != nil || message.Intervention == nil || *message.Intervention != *created.Intervention {
			t.Fatalf("message read actor %q: %+v %v", actor, message.Intervention, err)
		}
	}
	page, err := f.s.ListInterventions(f.ctx, f.task.ID, 0, 0, "")
	if err != nil || len(page.Interventions) != 3 {
		t.Fatalf("page: %+v %v", page, err)
	}
	for i, want := range []string{"owner", "helper", "unknown"} {
		if got := page.Interventions[i].Intervention; got == nil || got.Actor != want {
			t.Fatalf("page[%d] actor = %+v, want %s", i, got, want)
		}
	}
	board, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 0)
	if err != nil || len(board) != 3 || board[1].Intervention.Actor != "helper" {
		t.Fatalf("board read: %+v %v", board, err)
	}

	// The hub accepts only owner or helper; unknown is stored, never requested.
	before := countInterventionRows(t, f.s)
	for _, bad := range []string{"robot", "unknown", "Owner", "owner "} {
		req := f.request("actor-bad-"+strings.TrimSpace(bad), "nudge")
		req.Actor = bad
		if _, err = f.s.CreateIntervention(f.ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("actor %q err=%v", bad, err)
		}
	}
	// The same request ID with a different actor is a different payload.
	changed := f.request("actor-owner", "decision-on-behalf")
	changed.Actor = "helper"
	if _, err = f.s.CreateIntervention(f.ctx, f.task.ID, changed, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed actor err=%v", err)
	}
	if after := countInterventionRows(t, f.s); after != before {
		t.Fatalf("refused actors wrote rows: before=%v after=%v", before, after)
	}

	// A request without an actor hashes exactly as one sent before the field
	// existed, so a stored pre-upgrade receipt still replays instead of 409.
	old := f.request("actor-", "decision-on-behalf")
	legacy := preActorInterventionRequest{Kind: old.Kind, ItemID: old.ItemID, ProductItemID: old.ProductItemID, Text: old.Text, RequestID: old.RequestID}
	unknownSeq := page.Interventions[2].Seq
	if got, want := interventionReceiptHash(t, f.s, unknownSeq), operationRequestHash("owner_intervention", legacy); got != want {
		t.Fatalf("no-actor receipt hash %s, pre-upgrade shape hashes %s", got, want)
	}
	replayed, err := f.s.CreateIntervention(f.ctx, f.task.ID, old, f.by)
	if err != nil || replayed.Seq != unknownSeq || replayed.Intervention.Actor != "unknown" {
		t.Fatalf("pre-upgrade replay: %+v %v", replayed, err)
	}
	if after := countInterventionRows(t, f.s); after != before {
		t.Fatalf("replay wrote rows: before=%v after=%v", before, after)
	}
}

func interventionColumns(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('owner_interventions') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

// A database from before the actor column gains it on Open; its rows read
// unknown from the column default and are never rewritten.
func TestInterventionActorMigrationBackfillsUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, by := context.Background(), api.Caller{Node: "test-node", User: "owner"}
	task, _ := workItemProject(t, s, ctx, by, "Synthetic migration", "")
	item := createWorkItem(t, s, ctx, by, task, "migrated-item")
	var seqs []int64
	for _, key := range []string{"pre-actor-a", "pre-actor-b"} {
		m, err := s.CreateIntervention(ctx, task.ID, api.CreateInterventionRequest{Kind: "release", ItemID: item.ID, Text: "Released by hand", RequestID: key}, by)
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, m.Seq)
	}
	// Return the table to its pre-actor shape (the old schema had no actor).
	if _, err = s.db.Exec(`ALTER TABLE owner_interventions DROP COLUMN actor`); err != nil {
		t.Fatal(err)
	}
	wantOld := []string{"message_seq", "task_id", "kind", "item_task_id", "item_id", "product_task_id", "product_item_id", "created_at"}
	if got := interventionColumns(t, s); !reflect.DeepEqual(got, wantOld) {
		t.Fatalf("pre-actor columns = %v", got)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopening a pre-actor database: %v", err)
	}
	defer s.Close()
	fresh, err := Open(filepath.Join(dir, "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	freshColumns := interventionColumns(t, fresh)
	fresh.Close()
	if got := interventionColumns(t, s); !reflect.DeepEqual(got, freshColumns) || got[len(got)-1] != "actor" {
		t.Fatalf("migrated columns %v, fresh %v", got, freshColumns)
	}
	var actors []string
	rows, err := s.db.Query(`SELECT actor FROM owner_interventions ORDER BY message_seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			t.Fatal(err)
		}
		actors = append(actors, actor)
	}
	rows.Close()
	if !reflect.DeepEqual(actors, []string{"unknown", "unknown"}) {
		t.Fatalf("backfilled actors = %v", actors)
	}
	list, err := s.ListInterventions(ctx, task.ID, 0, 0, "")
	if err != nil || len(list.Interventions) != 2 || list.Interventions[0].Intervention.Actor != "unknown" || !reflect.DeepEqual(list.Summary.ByActor, map[string]int{"unknown": 2}) {
		t.Fatalf("migrated list: %+v %v", list, err)
	}
	// The immutability triggers survive the migration.
	for _, statement := range []string{
		`UPDATE owner_interventions SET actor='owner' WHERE message_seq=?`,
		`DELETE FROM owner_interventions WHERE message_seq=?`,
	} {
		if _, err := s.db.Exec(statement, seqs[0]); err == nil || !strings.Contains(err.Error(), "owner interventions are immutable") {
			t.Fatalf("%s after migration: err=%v", statement, err)
		}
	}
	// A pre-upgrade retry still replays the original record.
	replayed, err := s.CreateIntervention(ctx, task.ID, api.CreateInterventionRequest{Kind: "release", ItemID: item.ID, Text: "Released by hand", RequestID: "pre-actor-a"}, by)
	if err != nil || replayed.Seq != seqs[0] {
		t.Fatalf("pre-upgrade replay after migration: %+v %v", replayed, err)
	}
	m, err := s.CreateIntervention(ctx, task.ID, api.CreateInterventionRequest{Kind: "nudge", ItemID: item.ID, Text: "Nudged", RequestID: "post-actor", Actor: "helper"}, by)
	if err != nil || m.Intervention.Actor != "helper" {
		t.Fatalf("new helper record: %+v %v", m, err)
	}
	s.Close()

	// A further Open is a no-op: one actor column, rows unchanged.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('owner_interventions') WHERE name='actor'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("actor columns after a third Open: %d %v", n, err)
	}
	list, err = s.ListInterventions(ctx, task.ID, 0, 0, "")
	if err != nil || !reflect.DeepEqual(list.Summary.ByActor, map[string]int{"unknown": 2, "helper": 1}) {
		t.Fatalf("third open summary: %+v %v", list.Summary, err)
	}
}

func TestInterventionSummarySplitsByActorAndWindow(t *testing.T) {
	f := newInterventionFixture(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	clock := now
	f.s.now = func() time.Time { return clock }
	record := func(key, actor string, ago time.Duration, linked bool) api.Message {
		t.Helper()
		clock = now.Add(-ago)
		req := f.request(key, "nudge")
		req.Actor = actor
		if !linked {
			req.ProductItemID = ""
		}
		m, err := f.s.CreateIntervention(f.ctx, f.task.ID, req, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	unknown := record("split-unknown", "", 30*time.Hour, false)
	record("split-owner-7h", "owner", 7*time.Hour, true)
	record("split-helper-6h", "helper", 6*time.Hour, true) // exactly at the 6 h edge: outside it
	helper5 := record("split-helper-5h", "helper", 5*time.Hour, false)
	record("split-owner-1h", "owner", time.Hour, true)
	// Another project's interventions never leak into this summary.
	other, _ := workItemProject(t, f.s, f.ctx, f.by, "Synthetic split neighbour", "")
	otherItem := createWorkItem(t, f.s, f.ctx, f.by, other, "split-neighbour")
	clock = now.Add(-time.Hour)
	if _, err := f.s.CreateIntervention(f.ctx, other.ID, api.CreateInterventionRequest{Kind: "status", ItemID: otherItem.ID, Text: "Elsewhere", RequestID: "split-neighbour", Actor: "owner"}, f.by); err != nil {
		t.Fatal(err)
	}

	clock = now
	list, err := f.s.ListInterventions(f.ctx, f.task.ID, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	got := list.Summary
	if got.Total != 5 || !reflect.DeepEqual(got.ByActor, map[string]int{"owner": 2, "helper": 2, "unknown": 1}) {
		t.Fatalf("all-time split: total %d %v", got.Total, got.ByActor)
	}
	wantWindows := []api.InterventionWindow{
		{Hours: 6, Total: 2, ByActor: map[string]int{"owner": 1, "helper": 1}},
		{Hours: 24, Total: 4, ByActor: map[string]int{"owner": 2, "helper": 2}},
	}
	if !reflect.DeepEqual(got.Windows, wantWindows) {
		t.Fatalf("windows:\n got %+v\nwant %+v", got.Windows, wantWindows)
	}
	wantDays := []api.InterventionDay{
		{Day: "2026-10-28", Total: 4, Linked: 3, ByKind: map[string]int{"nudge": 4}, ByActor: map[string]int{"owner": 2, "helper": 2}},
		{Day: "2026-10-27", Total: 1, Linked: 0, ByKind: map[string]int{"nudge": 1}, ByActor: map[string]int{"unknown": 1}},
	}
	if !reflect.DeepEqual(got.Days, wantDays) {
		t.Fatalf("days:\n got %+v\nwant %+v", got.Days, wantDays)
	}
	wantUnlinked := []api.InterventionRef{
		{Seq: helper5.Seq, Kind: "nudge", ItemID: f.item.ID, Actor: "helper", Day: "2026-10-28"},
		{Seq: unknown.Seq, Kind: "nudge", ItemID: f.item.ID, Actor: "unknown", Day: "2026-10-27"},
	}
	if !reflect.DeepEqual(got.Unlinked, wantUnlinked) {
		t.Fatalf("unlinked:\n got %+v\nwant %+v", got.Unlinked, wantUnlinked)
	}

	// Windows move with the clock at read time; the all-time split does not.
	clock = now.Add(4 * time.Hour)
	later, err := f.s.ListInterventions(f.ctx, f.task.ID, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := later.Summary.Windows; w[0].Total != 1 || w[0].ByActor["owner"] != 1 || w[1].Total != 4 || !reflect.DeepEqual(later.Summary.ByActor, got.ByActor) {
		t.Fatalf("later windows: %+v", later.Summary)
	}

	// An empty project still sends every key.
	empty, _ := workItemProject(t, f.s, f.ctx, f.by, "Synthetic empty", "")
	none, err := f.s.ListInterventions(f.ctx, empty.ID, 0, 0, "")
	if err != nil || none.Summary.ByActor == nil || len(none.Summary.Windows) != 2 || none.Summary.Windows[0].ByActor == nil || none.Summary.Windows[1].Total != 0 {
		t.Fatalf("empty summary: %+v %v", none.Summary, err)
	}
}
