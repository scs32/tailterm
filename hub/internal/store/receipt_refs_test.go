package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

type receiptRefFixture struct {
	ctx   context.Context
	s     *Store
	by    api.Caller
	task  api.Task
	agent api.Agent
	a, b  api.WorkItem
	// idsA and idsB hold one saved receipt per prefix for items A and B.
	idsA, idsB map[string]string
}

func newReceiptRefFixture(t *testing.T) *receiptRefFixture {
	t.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "receipt-refs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &receiptRefFixture{ctx: ctx, s: s, by: api.Caller{Node: "receipt-ref-test", User: "owner"}}
	if f.task, err = s.CreateTask(ctx, api.CreateTaskRequest{Name: "receipt refs"}, f.by); err != nil {
		t.Fatal(err)
	}
	if f.agent, err = s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "sender", Host: "fixture", Session: "sender"}, f.by); err != nil {
		t.Fatal(err)
	}
	f.a = f.item(t, "a")
	f.b = f.item(t, "b")
	f.idsA = f.receipts(t, f.a)
	f.idsB = f.receipts(t, f.b)
	if f.a, err = s.GetWorkItem(ctx, f.task.ID, f.a.ID); err != nil {
		t.Fatal(err)
	}
	if f.b, err = s.GetWorkItem(ctx, f.task.ID, f.b.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *receiptRefFixture) item(t *testing.T, name string) api.WorkItem {
	t.Helper()
	item, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "receipt item " + name, Description: "fixture", RequestID: "item-" + name}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// receipts saves one receipt of each checked kind for an item: wir_ through
// the real update path, the rest as fixture rows in their owning tables.
func (f *receiptRefFixture) receipts(t *testing.T, item api.WorkItem) map[string]string {
	t.Helper()
	title := item.Title + " renamed"
	saved, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Title: &title, RequestID: "rename-" + item.ID}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{"wir": saved.Receipt.ID}
	for _, prefix := range []string{"wob", "nrr", "nart", "nlnk", "rel"} {
		ids[prefix] = api.NewID(prefix)
	}
	now := ts(f.s.now())
	stmts := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO work_order_bookkeeping(id,task_id,item_id,request_id,payload_hash,receipt_json,created_at) VALUES(?,?,?,?,?,?,?)`, []any{ids["wob"], f.task.ID, item.ID, "wob-" + item.ID, "h", "{}", now}},
		{`INSERT INTO narrative_receipts(id,task_id,item_id,operation,by_node,by_user,request_id,payload_hash,result,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, []any{ids["nrr"], f.task.ID, item.ID, "fixture", "n", "u", "nrr-" + item.ID, "h", "{}", now}},
		{`INSERT INTO narrative_artifacts(id,task_id,item_id,namespace,source_id,latest_version,created_at) VALUES(?,?,?,?,?,?,?)`, []any{ids["nart"], f.task.ID, item.ID, "fixture", "src", 1, now}},
		{`INSERT INTO narrative_artifact_versions(artifact_id,version,task_id,item_id,narrative_seq,source_version,kind,title,original_author,ingested_by,ingested_at,provenance,capture_state,availability) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, []any{ids["nart"], 1, f.task.ID, item.ID, 1, "v1", "note", "t", "{}", "{}", now, "native", "stored-content", "available"}},
		{`INSERT INTO narrative_links(id,task_id,item_id,latest_revision,created_at) VALUES(?,?,?,?,?)`, []any{ids["nlnk"], f.task.ID, item.ID, 1, now}},
		{`INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,?,?,?)`, []any{f.task.ID, ids["rel"], api.NewID("tqe"), "verified", 1, `{"itemId":"` + item.ID + `"}`}},
	}
	for _, st := range stmts {
		if _, err := f.s.db.ExecContext(f.ctx, st.query, st.args...); err != nil {
			t.Fatalf("%s: %v", st.query, err)
		}
	}
	return ids
}

func (f *receiptRefFixture) link(item api.WorkItem) []api.MessageWorkItem {
	return []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
}

func (f *receiptRefFixture) typed(refs map[string]string, links []api.MessageWorkItem, requestID string) api.PostMessageRequest {
	// A linked post carries a request ID.
	if requestID == "" && len(links) > 0 {
		requestID = api.NewID("ref")
	}
	return api.PostMessageRequest{AgentID: f.agent.ID, RunID: f.agent.RunID, RequestID: requestID, WorkItems: links, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Status of the receipt reference fixture", Refs: refs, Body: api.EnvelopeBody{Text: "fixture"}}}
}

func (f *receiptRefFixture) messages(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// refused posts req, requires a refusal naming every want string, and checks
// that no message was stored.
func (f *receiptRefFixture) refused(t *testing.T, req api.PostMessageRequest, want ...string) {
	t.Helper()
	before := f.messages(t)
	_, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	var envErr *api.EnvelopeError
	if !errors.Is(err, api.ErrInvalid) || !errors.As(err, &envErr) {
		t.Fatalf("refs %v: want an envelope refusal, got %v", req.Envelope.Refs, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("refusal %q does not name %q", err, w)
		}
	}
	if after := f.messages(t); after != before {
		t.Fatalf("refused post stored a message: %d -> %d", before, after)
	}
}

func (f *receiptRefFixture) accepted(t *testing.T, req api.PostMessageRequest) api.Message {
	t.Helper()
	m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		var refs map[string]string
		if req.Envelope != nil {
			refs = req.Envelope.Refs
		}
		t.Fatalf("refs %v: want accepted, got %v", refs, err)
	}
	return m
}

func TestReceiptRefUnknownReceiptRefused(t *testing.T) {
	f := newReceiptRefFixture(t)
	f.refused(t, f.typed(map[string]string{"receipt": "wob_4placeholder"}, nil, ""), "refs.receipt", "wob_4placeholder", "not a saved receipt")
	for _, prefix := range []string{"wob", "wir", "nrr", "nart", "nlnk", "rel"} {
		unknown := api.NewID(prefix)
		f.refused(t, f.typed(map[string]string{"receipt": unknown}, nil, ""), "refs.receipt", unknown)
		f.refused(t, f.typed(map[string]string{"order-receipt": unknown}, f.link(f.a), ""), "refs.order-receipt", unknown)
	}
	// Every bad ref is named, not only the first.
	f.refused(t, f.typed(map[string]string{"receipt": "wir_0000000000000000", "update-receipt": "rel_bad"}, nil, ""), "refs.receipt", "wir_0000000000000000", "refs.update-receipt", "rel_bad")
}

func TestReceiptRefSavedReceiptAccepted(t *testing.T) {
	f := newReceiptRefFixture(t)
	for prefix, id := range f.idsA {
		f.accepted(t, f.typed(map[string]string{"receipt": id}, nil, ""))
		f.accepted(t, f.typed(map[string]string{prefix + "-receipt": " " + id + " "}, f.link(f.a), ""))
	}
	f.accepted(t, f.typed(map[string]string{"receipt": f.idsA["nart"] + "@1"}, nil, ""))
	f.refused(t, f.typed(map[string]string{"receipt": f.idsA["nart"] + "@2"}, nil, ""), f.idsA["nart"]+"@2")
	f.refused(t, f.typed(map[string]string{"receipt": f.idsA["nart"] + "@x"}, nil, ""), f.idsA["nart"]+"@x")
}

func TestReceiptRefOtherItemRefused(t *testing.T) {
	f := newReceiptRefFixture(t)
	for _, id := range f.idsB {
		// Linked through --work-item.
		f.refused(t, f.typed(map[string]string{"receipt": id}, f.link(f.a), ""), id, f.b.ID, f.a.ID)
		// Linked through refs item, with and without a revision.
		f.refused(t, f.typed(map[string]string{"item": f.a.ID, "receipt": id}, nil, ""), id, "belongs to item "+f.b.ID, "not the linked item "+f.a.ID)
		f.refused(t, f.typed(map[string]string{"item": f.a.ID + "@2", "receipt": id}, nil, ""), id, f.b.ID, f.a.ID)
		// With no linked item, only existence is checked.
		f.accepted(t, f.typed(map[string]string{"receipt": id}, nil, ""))
		// A receipt of either linked item is accepted.
		f.accepted(t, f.typed(map[string]string{"item": f.b.ID, "receipt": id}, f.link(f.a), ""))
	}
	for _, id := range f.idsA {
		f.accepted(t, f.typed(map[string]string{"receipt": id}, f.link(f.a), ""))
		f.accepted(t, f.typed(map[string]string{"item": f.a.ID + "@2", "receipt": id}, nil, ""))
	}
}

func TestReceiptRefUncheckedRefsAccepted(t *testing.T) {
	f := newReceiptRefFixture(t)
	for _, refs := range []map[string]string{
		{"receipt-generation": "wob_4placeholder"},
		{"receipt1": "wir_0000000000000000"},
		{"order": "wob_4placeholder"},
		{"receipt": "mpr_4136f13eb6e9a8b6"},
		{"receipt": "/Users/fixture/artifacts/receipt.json"},
		{"receipt": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
		{"receipt": "bat_0000000000000000"},
		{"receipt": "gen6"},
	} {
		f.accepted(t, f.typed(refs, f.link(f.a), ""))
	}
	plain := f.accepted(t, api.PostMessageRequest{AgentID: f.agent.ID, RunID: f.agent.RunID, Text: "NOTICE: Plain text\nRefs: receipt=wob_4placeholder"})
	if plain.Envelope != nil {
		t.Fatalf("plain-text post became typed: %+v", plain.Envelope)
	}
}

func TestReceiptRefReplayAndRetryIdentity(t *testing.T) {
	f := newReceiptRefFixture(t)
	// A post accepted before its receipt stopped resolving still replays.
	req := f.typed(map[string]string{"receipt": f.idsA["wob"]}, f.link(f.a), "receipt-ref-replay")
	first := f.accepted(t, req)
	if _, err := f.s.db.ExecContext(f.ctx, `DELETE FROM work_order_bookkeeping WHERE id=?`, f.idsA["wob"]); err != nil {
		t.Fatal(err)
	}
	replay := f.accepted(t, req)
	if replay.Seq != first.Seq || replay.PostReceipt == nil {
		t.Fatalf("replay seq %d receipt %v, want seq %d", replay.Seq, replay.PostReceipt, first.Seq)
	}
	// A refused post saves no post receipt, so the corrected post may reuse its request ID.
	f.refused(t, f.typed(map[string]string{"receipt": "wob_4placeholder"}, f.link(f.a), "receipt-ref-retry"), "wob_4placeholder")
	corrected := f.accepted(t, f.typed(map[string]string{"receipt": f.idsA["wir"]}, f.link(f.a), "receipt-ref-retry"))
	if corrected.PostReceipt == nil || corrected.PostReceipt.RequestID != "receipt-ref-retry" {
		t.Fatalf("corrected post receipt %+v", corrected.PostReceipt)
	}
}
