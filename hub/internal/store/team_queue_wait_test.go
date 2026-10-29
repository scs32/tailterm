package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

// waitFixture is an isolated project with two queued entries: pred owns the
// shared path first and waiting is sequenced behind it. The clock is the
// test's: s.now and every sweep read f.clock.
type waitFixture struct {
	s             *Store
	ctx           context.Context
	by            api.Caller
	task          api.Task
	items         []api.WorkItem
	orders        []api.Message
	handler       api.Agent
	pred, waiting api.TeamQueueEntry
	lead, member  api.Agent
	clock         time.Time
}

const (
	waitAccepted = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	waitReleased = "cccccccccccccccccccccccccccccccccccccccc"
	waitPath     = "hub/cmd/tt/tool_ledger_test.go"
)

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()
	s, task, items, orders := sharedCheckoutFixture(t, 2)
	f := &waitFixture{s: s, ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}, task: task, items: items, orders: orders, clock: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return f.clock }
	agents, err := s.ListAgents(f.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Name == "database" {
			f.handler = a
		}
	}
	// Agents are judged live at the test's clock.
	f.exec(t, `UPDATE agents SET last_seen_at=? WHERE task_id=?`, ts(f.clock), task.ID)
	// The project has two live handlers and no explicit primary, so the
	// primary is the oldest by created_at, compared as text. ts trims
	// trailing zeros: created in the same millisecond, "….994Z" sorts after
	// "….994296Z" and aux-handler got the notices. Make database the oldest
	// by a whole day, and check it is the handler the hub tells.
	f.exec(t, `UPDATE agents SET created_at=? WHERE id=?`, ts(f.clock.Add(-24*time.Hour)), f.handler.ID)
	if told, err := queueWaitHandler(f.ctx, s.db, task); err != nil || told.ID == "" || told.ID != f.handler.ID {
		t.Fatalf("the fixture's handler is not the primary handler: %q, %v", told.Name, err)
	}
	f.pred = addCheckoutEntry(t, s, task, items[0], orders[0], "/worktrees/pred", waitPath)
	f.waiting = addCheckoutEntry(t, s, task, items[1], orders[1], "/worktrees/waiting", "hub/internal/redact")
	return f
}

func (f *waitFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// team gives the waiting item a live lead and a second member, both done
// with their turns, as a team that waits is.
func (f *waitFixture) team(t *testing.T) {
	t.Helper()
	add := func(name string) api.Agent {
		ref := api.MessageReference{TaskID: f.task.ID, Seq: f.orders[1].Seq}
		a, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: name, AgentID: api.NewID("agt"), Host: "mini", Session: name,
			WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.items[1].ID, ItemRevision: f.items[1].Revision, WorkOrderMessage: ref, ContextBundle: syntheticPreparedContext(t, f.items[1], ref, syntheticHistory(f.items[1], f.orders[1]))}}, f.by)
		if err != nil {
			t.Fatalf("admit %s: %v", name, err)
		}
		f.exec(t, `UPDATE agents SET status='done',last_seen_at=? WHERE id=?`, ts(f.clock), a.ID)
		return a
	}
	f.lead, f.member = add("waiting-lead"), add("waiting-builder")
	f.exec(t, `INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, f.task.ID, f.items[1].ID, f.lead.ID, f.lead.RunID)
}

func (f *waitFixture) helper(t *testing.T) api.Agent {
	t.Helper()
	a, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "owner-helper", AgentID: api.NewID("agt"), Host: "mini", Session: "owner-helper"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE agents SET role=?,status='running' WHERE id=?`, api.AgentRoleOwnerHelper, a.ID)
	return a
}

func (f *waitFixture) setRequest(until string, paths ...string) api.TeamQueueRequest {
	if len(paths) == 0 {
		paths = []string{waitPath}
	}
	return api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "wait_set", EntryID: f.waiting.ID, WaitOnEntryID: f.pred.ID, WaitPaths: paths, WaitUntil: until}
}

// setPause is a pathless wait: a reason in place of paths.
func (f *waitFixture) setPause(until, reason string) api.TeamQueueRequest {
	return api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "wait_set", EntryID: f.waiting.ID, WaitOnEntryID: f.pred.ID, WaitUntil: until, WaitReason: reason}
}

func (f *waitFixture) set(t *testing.T, until string) api.TeamQueueEntry {
	t.Helper()
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setRequest(until))
	if err != nil {
		t.Fatalf("wait set: %v", err)
	}
	return q
}

func (f *waitFixture) clear(t *testing.T) api.TeamQueueEntry {
	t.Helper()
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "wait_clear", EntryID: f.waiting.ID, WaitOnEntryID: f.pred.ID, WaitReason: "sequencing changed"})
	if err != nil {
		t.Fatalf("wait clear: %v", err)
	}
	return q
}

// sweep runs one wait sweep at the fixture clock and returns its actions.
func (f *waitFixture) sweep(t *testing.T) []string {
	t.Helper()
	steps, err := f.s.QueueWaitSweep(f.ctx, f.clock)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var out []string
	for _, step := range steps {
		if step.TaskID != f.task.ID || step.EntryID != f.waiting.ID || step.OnEntryID != f.pred.ID || step.MessageSeq == 0 {
			t.Fatalf("step %+v", step)
		}
		out = append(out, step.Action)
	}
	return out
}

func (f *waitFixture) state(t *testing.T) string {
	t.Helper()
	var state string
	if err := f.s.db.QueryRow(`SELECT state FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, f.task.ID, f.waiting.ID, f.pred.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// notices returns the hub's wait notices with this subject, sent to the agent.
func (f *waitFixture) notices(t *testing.T, to api.Agent, subject string) []api.Message {
	t.Helper()
	msgs, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.Envelope != nil && m.Envelope.Subject == subject && m.To == to.ID && m.From.Node == api.BrokerNode {
			out = append(out, m)
		}
	}
	return out
}

func (f *waitFixture) messageCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *waitFixture) waitingEntry(t *testing.T) api.TeamQueueEntry {
	t.Helper()
	list, err := f.s.ListTeamQueue(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Entries {
		if e.ID == f.waiting.ID {
			return e
		}
	}
	t.Fatal("waiting entry is not listed")
	return api.TeamQueueEntry{}
}

// reopen closes the store and opens the same file, as a hub restart does.
func (f *waitFixture) reopen(t *testing.T) {
	t.Helper()
	var seq int
	var name, path string
	if err := f.s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.now = func() time.Time { return f.clock }
	f.s = s
}

// meet makes the predecessor meet a condition, as the rows the accept
// transaction, the done save and the release finish leave behind.
func (f *waitFixture) meet(t *testing.T, until string) {
	t.Helper()
	switch until {
	case api.TeamQueueWaitAccepted:
		f.exec(t, `UPDATE team_queue_entries SET acceptance_json=? WHERE id=?`, `{"commit":"`+waitAccepted+`"}`, f.pred.ID)
	case api.TeamQueueWaitDone:
		f.exec(t, `UPDATE work_items SET status='done' WHERE task_id=? AND id=?`, f.task.ID, f.items[0].ID)
	case api.TeamQueueWaitReleased:
		f.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'released',1,?)`, f.task.ID, api.NewID("rel"), f.pred.ID, `{"commit":"`+waitAccepted+`","integratedCommit":"`+waitReleased+`"}`)
	}
}

// a1: the owner or a live database handler records one wait, shown under the
// waiting entry; the entry row and its revision are untouched.
func TestQueueWaitSetByOwnerAndHandler(t *testing.T) {
	f := newWaitFixture(t)
	q := f.set(t, api.TeamQueueWaitAccepted)
	if len(q.Waits) != 1 || q.Revision != f.waiting.Revision || q.ID != f.waiting.ID {
		t.Fatalf("set result %+v", q)
	}
	w := q.Waits[0]
	if w.OnItemID != f.items[0].ID || w.OnEntryID != f.pred.ID || len(w.Paths) != 1 || w.Paths[0] != waitPath || w.Until != api.TeamQueueWaitAccepted || w.State != api.TeamQueueWaitWaiting {
		t.Fatalf("wait %+v", w)
	}
	listed := f.waitingEntry(t)
	if len(listed.Waits) != 1 || listed.Waits[0].State != api.TeamQueueWaitWaiting || listed.Revision != f.waiting.Revision {
		t.Fatalf("listed %+v", listed)
	}
	var by string
	if err := f.s.db.QueryRow(`SELECT set_by FROM team_queue_waits WHERE entry_id=?`, f.waiting.ID).Scan(&by); err != nil || by != "owner" {
		t.Fatalf("set by %q %v", by, err)
	}
	f.clear(t)

	req := f.setRequest(api.TeamQueueWaitReleased)
	req.HandlerAgentID, req.HandlerRunID = f.handler.ID, f.handler.RunID
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil || len(q.Waits) != 1 || q.Waits[0].Until != api.TeamQueueWaitReleased {
		t.Fatalf("handler set %+v %v", q, err)
	}
	if err := f.s.db.QueryRow(`SELECT set_by FROM team_queue_waits WHERE entry_id=?`, f.waiting.ID).Scan(&by); err != nil || by != f.handler.ID {
		t.Fatalf("set by %q %v", by, err)
	}
	// A lost response is replayed by its request ID.
	again, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil || len(again.Waits) != 1 {
		t.Fatalf("replay %+v %v", again, err)
	}
	var rows int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_waits`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows %d %v", rows, err)
	}
}

// a1: each refusal names its reason, and none stores a wait.
func TestQueueWaitSetRefusals(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	other, err := f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Another project"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	foreignItem, err := f.s.CreateWorkItem(f.ctx, other.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Elsewhere", Priority: "normal", RequestID: api.NewID("req")}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	foreign := "tqe_" + strings.Repeat("f", 16)
	f.exec(t, `INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,created_at,updated_at) VALUES(?,?,?,1,1,'planned',1,'queued',1,'mini','/x',?,?)`, foreign, other.ID, foreignItem.ID, ts(f.clock), ts(f.clock))
	for name, c := range map[string]struct {
		edit func(*api.TeamQueueRequest)
		kind error
		text string
	}{
		"an item lead":                   {func(r *api.TeamQueueRequest) { r.LeadAgentID, r.LeadRunID = f.lead.ID, f.lead.RunID }, api.ErrConflict, "only the owner or a database handler may set or clear a shared-path wait"},
		"a handler run that is not live": {func(r *api.TeamQueueRequest) { r.HandlerAgentID, r.HandlerRunID = f.handler.ID, "run_stale" }, api.ErrConflict, "only the owner or a database handler"},
		"an ordinary agent as handler":   {func(r *api.TeamQueueRequest) { r.HandlerAgentID, r.HandlerRunID = f.member.ID, f.member.RunID }, api.ErrConflict, "only the owner or a database handler"},
		"an entry on itself":             {func(r *api.TeamQueueRequest) { r.WaitOnEntryID = r.EntryID }, api.ErrInvalid, "an entry cannot wait on itself"},
		"an unknown predecessor":         {func(r *api.TeamQueueRequest) { r.WaitOnEntryID = "tqe_" + strings.Repeat("0", 16) }, api.ErrConflict, "is not in this project's queue"},
		"another project's predecessor":  {func(r *api.TeamQueueRequest) { r.WaitOnEntryID = foreign }, api.ErrConflict, "is not in this project's queue"},
		"an unknown condition":           {func(r *api.TeamQueueRequest) { r.WaitUntil = "merged" }, api.ErrInvalid, "accepted, done or released"},
		"no condition":                   {func(r *api.TeamQueueRequest) { r.WaitUntil = "" }, api.ErrInvalid, "accepted, done or released"},
		"no path":                        {func(r *api.TeamQueueRequest) { r.WaitPaths = nil }, api.ErrInvalid, "at least one path"},
		"an absolute path":               {func(r *api.TeamQueueRequest) { r.WaitPaths = []string{"/etc/passwd"} }, api.ErrInvalid, "invalid ownership path"},
		"a malformed predecessor":        {func(r *api.TeamQueueRequest) { r.WaitOnEntryID = "wi_nope" }, api.ErrInvalid, "both tqe_ID"},
	} {
		req := f.setRequest(api.TeamQueueWaitAccepted)
		c.edit(&req)
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req); !errors.Is(err, c.kind) || !strings.Contains(err.Error(), c.text) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	unknown := f.setRequest(api.TeamQueueWaitAccepted)
	unknown.EntryID = "tqe_" + strings.Repeat("1", 16)
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, unknown); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown waiting entry: %v", err)
	}
	// A finished waiting entry, and a failed one that was released, cannot wait.
	for _, ended := range []struct{ state, released, text string }{{"finished", "", "is finished"}, {"failed", ts(f.clock), "is failed and released"}} {
		f.exec(t, `UPDATE team_queue_entries SET state=?,released_at=? WHERE id=?`, ended.state, ended.released, f.waiting.ID)
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setRequest(api.TeamQueueWaitAccepted)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), ended.text) {
			t.Fatalf("%s waiting entry: %v", ended.state, err)
		}
	}
	var rows int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_waits`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused waits stored %d rows %v", rows, err)
	}
}

// a1 and a2: an identical repeat changes nothing, a different wait on the
// same pair is a conflict that names the clear command, clear removes the
// wait from the listing, and a cleared pair can be set again.
func TestQueueWaitRepeatConflictAndClear(t *testing.T) {
	f := newWaitFixture(t)
	f.set(t, api.TeamQueueWaitAccepted)
	var setAt string
	if err := f.s.db.QueryRow(`SELECT set_at FROM team_queue_waits`).Scan(&setAt); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Minute)
	if q := f.set(t, api.TeamQueueWaitAccepted); len(q.Waits) != 1 || q.Waits[0].SetAt != setAt {
		t.Fatalf("identical repeat changed the wait: %+v", q.Waits)
	}
	clearCommand := fmt.Sprintf("tt team queue wait clear --task %s --entry %s --on %s", f.task.ID, f.waiting.ID, f.pred.ID)
	for name, req := range map[string]api.TeamQueueRequest{
		"another condition": f.setRequest(api.TeamQueueWaitReleased),
		"other paths":       f.setRequest(api.TeamQueueWaitAccepted, "hub/cmd/tt/main.go"),
	} {
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), clearCommand) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	sentence := fmt.Sprintf("waits on %s for %s until accepted", f.items[0].ID, waitPath)
	if listed := f.waitingEntry(t); len(listed.Waits) != 1 || listed.Waits[0].Text() != sentence || !strings.HasSuffix(listed.BlockReason, sentence) {
		t.Fatalf("listed %+v reason %q", listed.Waits, listed.BlockReason)
	}

	lead := api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "wait_clear", EntryID: f.waiting.ID, WaitOnEntryID: f.pred.ID, LeadAgentID: "agt_lead", LeadRunID: "run_lead"}
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, lead); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("a lead cleared a wait: %v", err)
	}
	if q := f.clear(t); len(q.Waits) != 0 {
		t.Fatalf("clear result still waits: %+v", q.Waits)
	}
	if listed := f.waitingEntry(t); len(listed.Waits) != 0 || strings.Contains(listed.BlockReason, "waits on") {
		t.Fatalf("cleared wait still listed: %+v %q", listed.Waits, listed.BlockReason)
	}
	var state, reason string
	if err := f.s.db.QueryRow(`SELECT state,reason FROM team_queue_waits`).Scan(&state, &reason); err != nil || state != api.TeamQueueWaitCleared || reason != "sequencing changed" {
		t.Fatalf("cleared row %s %q %v", state, reason, err)
	}
	again := api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "wait_clear", EntryID: f.waiting.ID, WaitOnEntryID: f.pred.ID}
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, again); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("clearing a cleared wait: %v", err)
	}
	if q := f.set(t, api.TeamQueueWaitDone); len(q.Waits) != 1 || q.Waits[0].Until != api.TeamQueueWaitDone || q.Waits[0].State != api.TeamQueueWaitWaiting {
		t.Fatalf("set after clear: %+v", q.Waits)
	}
}

// a3: accepted, done and released each post exactly one notice to the
// waiting lead, with the predecessor, condition, commit and paths; ten more
// sweeps and a reopened store post no second one.
func TestQueueWaitMetNotifiesLeadOnce(t *testing.T) {
	for _, c := range []struct{ until, commit, shown string }{
		{api.TeamQueueWaitAccepted, waitAccepted, "accepted commit " + waitAccepted},
		{api.TeamQueueWaitDone, "", "none recorded"},
		{api.TeamQueueWaitReleased, waitReleased, "released commit " + waitReleased},
	} {
		t.Run(c.until, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			f.set(t, c.until)
			if steps := f.sweep(t); len(steps) != 0 {
				t.Fatalf("steps before the condition: %v", steps)
			}
			f.meet(t, c.until)
			if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
				t.Fatalf("steps at the condition: %v", steps)
			}
			got := f.notices(t, f.lead, queueWaitMetSubject)
			if len(got) != 1 {
				t.Fatalf("%d notices to the lead", len(got))
			}
			text, refs := got[0].Envelope.Body.Text, got[0].Envelope.Refs
			for _, want := range []string{f.items[0].ID, f.pred.ID, "until it was " + c.until, c.shown, "Paths now free: " + waitPath, "before editing these paths"} {
				if !strings.Contains(text, want) {
					t.Fatalf("notice lacks %q: %s", want, text)
				}
			}
			wantCommit := c.commit
			if wantCommit == "" {
				wantCommit = "none recorded"
			}
			if refs["activity"] != "queue_wait" || refs["predecessor"] != f.items[0].ID || refs["predecessorEntry"] != f.pred.ID || refs["condition"] != c.until || refs["commit"] != wantCommit || refs["entry"] != f.waiting.ID {
				t.Fatalf("refs %v", refs)
			}
			listed := f.waitingEntry(t)
			if len(listed.Waits) != 1 || listed.Waits[0].State != api.TeamQueueWaitMet || listed.Waits[0].NoticeSeq != got[0].Seq || listed.Waits[0].MetCommit != c.commit ||
				!strings.Contains(listed.BlockReason, fmt.Sprintf("wait on %s met (%s, commit %s): rebase before editing %s", f.items[0].ID, c.until, wantCommit, waitPath)) {
				t.Fatalf("listed %+v %q", listed.Waits, listed.BlockReason)
			}
			for i := 0; i < 5; i++ {
				f.clock = f.clock.Add(30 * time.Second)
				if steps := f.sweep(t); len(steps) != 0 {
					t.Fatalf("sweep %d repeated: %v", i, steps)
				}
			}
			f.reopen(t)
			for i := 0; i < 5; i++ {
				f.clock = f.clock.Add(30 * time.Second)
				if steps := f.sweep(t); len(steps) != 0 {
					t.Fatalf("sweep %d after reopen repeated: %v", i, steps)
				}
			}
			if got := f.notices(t, f.lead, queueWaitMetSubject); len(got) != 1 {
				t.Fatalf("%d notices to the lead after ten sweeps and a reopen", len(got))
			}
		})
	}
}

// a3: done names the accepted commit when the predecessor has one.
func TestQueueWaitDoneNamesAcceptedCommit(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	f.set(t, api.TeamQueueWaitDone)
	f.meet(t, api.TeamQueueWaitAccepted)
	if steps := f.sweep(t); len(steps) != 0 {
		t.Fatalf("acceptance met a done wait: %v", steps)
	}
	f.meet(t, api.TeamQueueWaitDone)
	if steps := f.sweep(t); len(steps) != 1 {
		t.Fatalf("steps %v", steps)
	}
	got := f.notices(t, f.lead, queueWaitMetSubject)
	if len(got) != 1 || !strings.Contains(got[0].Envelope.Body.Text, "accepted commit "+waitAccepted) || !strings.Contains(got[0].Envelope.Body.Text, "Rebase onto that commit") {
		t.Fatalf("notice %+v", got)
	}
}

// A waiting entry that is still queued has no lead: the wait stays waiting
// and the lead is told once it exists.
func TestQueueWaitMetWaitsForALead(t *testing.T) {
	f := newWaitFixture(t)
	f.set(t, api.TeamQueueWaitAccepted)
	f.meet(t, api.TeamQueueWaitAccepted)
	before := f.messageCount(t)
	for i := 0; i < 3; i++ {
		if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitWaiting {
			t.Fatalf("steps without a lead: %v, state %s", steps, f.state(t))
		}
	}
	if after := f.messageCount(t); after != before {
		t.Fatalf("%d messages posted without a lead", after-before)
	}
	f.team(t)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
		t.Fatalf("steps with a lead: %v", steps)
	}
	if got := f.notices(t, f.lead, queueWaitMetSubject); len(got) != 1 {
		t.Fatalf("%d notices", len(got))
	}
}

// a4: a team idle for the bound after its wait was met is reported once to
// the owner helper and once to the primary handler: not one second early
// and not again.
func TestQueueWaitIdleTeamReportedOnce(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	helper := f.helper(t)
	f.set(t, api.TeamQueueWaitAccepted)
	f.meet(t, api.TeamQueueWaitAccepted)
	f.sweep(t)
	met := f.clock
	f.clock = met.Add(15*time.Minute - time.Second)
	if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitMet {
		t.Fatalf("one second early: %v, state %s", steps, f.state(t))
	}
	f.clock = met.Add(15 * time.Minute)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-overdue" {
		t.Fatalf("at the bound: %v", steps)
	}
	notice := f.notices(t, f.lead, queueWaitMetSubject)[0]
	for _, to := range []api.Agent{helper, f.handler} {
		got := f.notices(t, to, queueWaitOverdueSubject)
		if len(got) != 1 {
			t.Fatalf("%d overdue notices to %s", len(got), to.Name)
		}
		text, refs := got[0].Envelope.Body.Text, got[0].Envelope.Refs
		if !strings.Contains(text, f.waiting.ID) || !strings.Contains(text, f.items[0].ID) || !strings.Contains(text, "idle for 15 minutes") || !strings.Contains(text, waitPath) ||
			refs["activity"] != "queue_wait_overdue" || refs["notice"] != fmt.Sprint(notice.Seq) || refs["entry"] != f.waiting.ID {
			t.Fatalf("overdue notice %s %v", text, refs)
		}
	}
	if f.state(t) != api.TeamQueueWaitOverdue {
		t.Fatalf("state %s", f.state(t))
	}
	f.reopen(t)
	for i := 0; i < 10; i++ {
		f.clock = f.clock.Add(time.Minute)
		if steps := f.sweep(t); len(steps) != 0 {
			t.Fatalf("sweep %d reported again: %v", i, steps)
		}
	}
	if n := len(f.notices(t, helper, queueWaitOverdueSubject)) + len(f.notices(t, f.handler, queueWaitOverdueSubject)); n != 2 {
		t.Fatalf("%d overdue notices after ten more sweeps", n)
	}
	if got := f.notices(t, f.lead, queueWaitOverdueSubject); len(got) != 0 {
		t.Fatalf("the waiting lead got the overdue report")
	}
	if listed := f.waitingEntry(t); len(listed.Waits) != 0 {
		t.Fatalf("a reported wait is still listed: %+v", listed.Waits)
	}
}

// a4: a member who posts after the wait notice, or who is working, ends the
// wait as resumed; the team is never reported for it.
func TestQueueWaitTeamActivityEndsTheWait(t *testing.T) {
	for name, act := range map[string]func(*testing.T, *waitFixture){
		"a member posts": func(t *testing.T, f *waitFixture) {
			if _, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.member.ID, Text: "rebased, editing now", RequestID: api.NewID("req")}, f.by); err != nil {
				t.Fatal(err)
			}
		},
		"a member is working": func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE agents SET status='running' WHERE id=?`, f.lead.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			helper := f.helper(t)
			f.set(t, api.TeamQueueWaitAccepted)
			f.meet(t, api.TeamQueueWaitAccepted)
			f.sweep(t)
			f.clock = f.clock.Add(time.Minute)
			act(t, f)
			if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitResumed {
				t.Fatalf("after activity: %v, state %s", steps, f.state(t))
			}
			// Idle again, long past the bound: the wait is final.
			f.exec(t, `UPDATE agents SET status='done' WHERE id=?`, f.lead.ID)
			f.clock = f.clock.Add(time.Hour)
			if steps := f.sweep(t); len(steps) != 0 {
				t.Fatalf("a resumed wait was reported: %v", steps)
			}
			if n := len(f.notices(t, helper, queueWaitOverdueSubject)) + len(f.notices(t, f.handler, queueWaitOverdueSubject)); n != 0 {
				t.Fatalf("%d overdue notices for an active team", n)
			}
		})
	}
}

// a4: the hub's environment moves the bound, and a value outside 1..1440
// keeps the default.
func TestQueueWaitIdleBoundFromEnvironment(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 15 * time.Minute, "5": 5 * time.Minute, "1440": 24 * time.Hour, "0": 15 * time.Minute, "1441": 15 * time.Minute, "soon": 15 * time.Minute} {
		t.Setenv("TAILTERM_QUEUE_WAIT_IDLE_MINUTES", value)
		if got := queueWaitIdleBound(); got != want {
			t.Fatalf("bound for %q = %s, want %s", value, got, want)
		}
	}
	t.Setenv("TAILTERM_QUEUE_WAIT_IDLE_MINUTES", "5")
	f := newWaitFixture(t)
	f.team(t)
	helper := f.helper(t)
	f.set(t, api.TeamQueueWaitAccepted)
	f.meet(t, api.TeamQueueWaitAccepted)
	f.sweep(t)
	met := f.clock
	f.clock = met.Add(5*time.Minute - time.Second)
	if steps := f.sweep(t); len(steps) != 0 {
		t.Fatalf("one second before five minutes: %v", steps)
	}
	f.clock = met.Add(5 * time.Minute)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-overdue" {
		t.Fatalf("at five minutes: %v", steps)
	}
	if got := f.notices(t, helper, queueWaitOverdueSubject); len(got) != 1 || !strings.Contains(got[0].Envelope.Body.Text, "idle for 5 minutes") {
		t.Fatalf("overdue notice %+v", got)
	}
}

// a5: a predecessor that can no longer meet the condition is reported once
// to the primary handler with the reason and both commands. The lead is told
// nothing, the wait is not met, and the listing says a decision is needed.
func TestQueueWaitOrphanedReportedToHandler(t *testing.T) {
	ownerCommit := strings.Repeat("d", 40)
	for name, c := range map[string]struct {
		until  string
		orphan func(*testing.T, *waitFixture)
		reason string
	}{
		"removed": {api.TeamQueueWaitAccepted, func(t *testing.T, f *waitFixture) {
			if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "remove", EntryID: f.pred.ID, ExpectedRevision: f.pred.Revision}); err != nil {
				t.Fatal(err)
			}
		}, "the predecessor entry was removed from the queue"},
		"failed and released": {api.TeamQueueWaitAccepted, func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='failed',released_at=? WHERE id=?`, ts(f.clock), f.pred.ID)
		}, "the predecessor entry failed and was released"},
		"owner integrated": {api.TeamQueueWaitReleased, func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='running',released_at=?,owner_integration_json=? WHERE id=?`, ts(f.clock), `{"commit":"`+ownerCommit+`"}`, f.pred.ID)
		}, "the owner integrated the predecessor entry outside the acceptance path as commit " + ownerCommit},
		"finished without acceptance": {api.TeamQueueWaitAccepted, func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
		}, "the predecessor entry finished without acceptance"},
		"finished with its item open": {api.TeamQueueWaitDone, func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
		}, "the predecessor entry finished and its item is not saved done"},
		"finished without a release job": {api.TeamQueueWaitReleased, func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
		}, "the predecessor entry finished without a release job"},
		"item dismissed": {api.TeamQueueWaitDone, func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE work_items SET status='dismissed' WHERE task_id=? AND id=?`, f.task.ID, f.items[0].ID)
		}, "the predecessor item was dismissed"},
		"release superseded": {api.TeamQueueWaitReleased, func(t *testing.T, f *waitFixture) {
			f.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'superseded',1,'{}')`, f.task.ID, api.NewID("rel"), f.pred.ID)
		}, "the predecessor's release job is superseded"},
		"release rolled back": {api.TeamQueueWaitReleased, func(t *testing.T, f *waitFixture) {
			f.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'rolled_back',1,'{}')`, f.task.ID, api.NewID("rel"), f.pred.ID)
		}, "the predecessor's release job is rolled back"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			helper := f.helper(t)
			f.set(t, c.until)
			c.orphan(t, f)
			if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-orphaned" {
				t.Fatalf("steps %v", steps)
			}
			got := f.notices(t, f.handler, queueWaitOrphanedSubject)
			if len(got) != 1 {
				t.Fatalf("%d notices to the handler", len(got))
			}
			text := got[0].Envelope.Body.Text
			for _, want := range []string{c.reason, f.waiting.ID, f.items[0].ID,
				fmt.Sprintf("tt team queue wait clear --task %s --entry %s --on %s", f.task.ID, f.waiting.ID, f.pred.ID),
				fmt.Sprintf("tt team queue wait set --task %s --entry %s --on PREDECESSOR_ENTRY", f.task.ID, f.waiting.ID)} {
				if !strings.Contains(text, want) {
					t.Fatalf("handler notice lacks %q: %s", want, text)
				}
			}
			if f.state(t) != api.TeamQueueWaitOrphaned {
				t.Fatalf("state %s", f.state(t))
			}
			sentence := fmt.Sprintf("wait on %s needs a handler decision: %s", f.items[0].ID, c.reason)
			if listed := f.waitingEntry(t); len(listed.Waits) != 1 || listed.Waits[0].State != api.TeamQueueWaitOrphaned || !strings.HasSuffix(listed.BlockReason, sentence) {
				t.Fatalf("listed %+v %q", listed.Waits, listed.BlockReason)
			}
			f.reopen(t)
			for i := 0; i < 10; i++ {
				f.clock = f.clock.Add(time.Minute)
				if steps := f.sweep(t); len(steps) != 0 {
					t.Fatalf("sweep %d reported again: %v", i, steps)
				}
			}
			if got := f.notices(t, f.handler, queueWaitOrphanedSubject); len(got) != 1 {
				t.Fatalf("%d notices to the handler after ten more sweeps", len(got))
			}
			for _, to := range []api.Agent{f.lead, f.member, helper} {
				msgs, err := f.s.ListMessages(f.ctx, f.task.ID, 0, to.ID, 100)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range msgs {
					if m.To == to.ID {
						t.Fatalf("%s was told: %s", to.Name, m.Text)
					}
				}
			}
			// The handler's decision: clear, then a new wait can be set.
			f.clear(t)
			if listed := f.waitingEntry(t); len(listed.Waits) != 0 {
				t.Fatalf("cleared orphan still listed: %+v", listed.Waits)
			}
		})
	}
}

// a5: a running predecessor, a failed one not yet released, and a release
// job still in flight are pending, not orphaned.
func TestQueueWaitPendingPredecessorIsNotReported(t *testing.T) {
	for name, pend := range map[string]func(*testing.T, *waitFixture){
		"running": func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='running' WHERE id=?`, f.pred.ID)
		},
		"failed and not released": func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='failed' WHERE id=?`, f.pred.ID)
		},
		"finished with a verified release job": func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
			f.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'verified',1,'{}')`, f.task.ID, api.NewID("rel"), f.pred.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			f.set(t, api.TeamQueueWaitReleased)
			pend(t, f)
			before := f.messageCount(t)
			if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitWaiting || f.messageCount(t) != before {
				t.Fatalf("steps %v, state %s", steps, f.state(t))
			}
		})
	}
}

// The latest release job decides: a refused job followed by a released one
// meets the wait.
func TestQueueWaitReadsLatestReleaseJob(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	f.set(t, api.TeamQueueWaitReleased)
	f.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'refused',1,'{}')`, f.task.ID, api.NewID("rel"), f.pred.ID)
	f.meet(t, api.TeamQueueWaitReleased)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
		t.Fatalf("steps %v", steps)
	}
}

// a6: with no wait recorded, a sweep over a project whose entry is
// accepted, done and released posts no message and writes no row.
func TestQueueWaitSweepWithoutAWait(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	f.helper(t)
	for _, until := range []string{api.TeamQueueWaitAccepted, api.TeamQueueWaitDone, api.TeamQueueWaitReleased} {
		f.meet(t, until)
	}
	before := f.messageCount(t)
	for i := 0; i < 3; i++ {
		f.clock = f.clock.Add(20 * time.Minute)
		if steps := f.sweep(t); len(steps) != 0 {
			t.Fatalf("steps without a wait: %v", steps)
		}
	}
	var rows int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_waits`).Scan(&rows); err != nil || rows != 0 || f.messageCount(t) != before {
		t.Fatalf("rows %d, %d new messages, %v", rows, f.messageCount(t)-before, err)
	}
	if listed := f.waitingEntry(t); len(listed.Waits) != 0 || strings.Contains(listed.BlockReason, "wait") {
		t.Fatalf("listed %+v %q", listed.Waits, listed.BlockReason)
	}
}

// a7: a paused project's waits are not evaluated until it resumes.
func TestQueueWaitPausedProjectIsNotEvaluated(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	f.set(t, api.TeamQueueWaitAccepted)
	f.meet(t, api.TeamQueueWaitAccepted)
	f.exec(t, `UPDATE tasks SET pause_state=? WHERE id=?`, api.ProjectPausePaused, f.task.ID)
	before := f.messageCount(t)
	for i := 0; i < 3; i++ {
		if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitWaiting {
			t.Fatalf("paused project evaluated: %v, state %s", steps, f.state(t))
		}
	}
	if f.messageCount(t) != before {
		t.Fatal("a paused project got a wait notice")
	}
	f.exec(t, `UPDATE tasks SET pause_state=? WHERE id=?`, api.ProjectPauseActive, f.task.ID)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
		t.Fatalf("after resume: %v", steps)
	}
}

// A wait whose own entry finished, was released as failed or was removed is
// cleared without a message.
func TestQueueWaitClearedWhenWaitingEntryEnds(t *testing.T) {
	for name, end := range map[string]func(*testing.T, *waitFixture){
		"finished": func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.waiting.ID)
		},
		"failed and released": func(t *testing.T, f *waitFixture) {
			f.exec(t, `UPDATE team_queue_entries SET state='failed',released_at=? WHERE id=?`, ts(f.clock), f.waiting.ID)
		},
		"removed": func(t *testing.T, f *waitFixture) {
			if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "remove", EntryID: f.waiting.ID, ExpectedRevision: f.waiting.Revision}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.set(t, api.TeamQueueWaitAccepted)
			f.meet(t, api.TeamQueueWaitAccepted)
			end(t, f)
			before := f.messageCount(t)
			if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitCleared || f.messageCount(t) != before {
				t.Fatalf("steps %v, state %s", steps, f.state(t))
			}
		})
	}
}

// A long path list is cut to fit the notice body.
func TestQueueWaitNoticeFitsManyPaths(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	var paths []string
	for i := 0; i < 200; i++ {
		paths = append(paths, fmt.Sprintf("hub/internal/store/a_rather_long_generated_file_name_%03d_test.go", i))
	}
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setRequest(api.TeamQueueWaitAccepted, paths...)); err != nil {
		t.Fatal(err)
	}
	f.meet(t, api.TeamQueueWaitAccepted)
	if steps := f.sweep(t); len(steps) != 1 {
		t.Fatalf("steps %v", steps)
	}
	got := f.notices(t, f.lead, queueWaitMetSubject)
	if len(got) != 1 || !strings.Contains(got[0].Envelope.Body.Text, " more. Rebase") || len(got[0].Envelope.Body.Text) > api.MaxEnvelopeBodyBytes {
		t.Fatalf("notice %+v", got)
	}
}

// Review b1: a wait with many paths keeps the listed sentence and the
// entry's blockReason small, so the stall notice that carries blockReason
// still posts; the JSON waits list keeps every path.
func TestQueueWaitManyPathsKeepStallNoticePostable(t *testing.T) {
	s, task, items, orders := sharedCheckoutFixture(t, 2)
	ctx := context.Background()
	s.queueStallGrace = time.Nanosecond
	a := addCheckoutEntry(t, s, task, items[0], orders[0], "/main", "src/a")
	b := addCheckoutEntry(t, s, task, items[1], orders[1], "/main", "src/b")
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "claim-a", Operation: "claim", EntryID: a.ID, ExpectedRevision: a.Revision, Host: "mini"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	before := listedEntry(t, s, task.ID, b.ID)
	if before.Stall == nil {
		t.Fatalf("no stall: %q", before.BlockReason)
	}
	var owns []string
	for i := 0; i < 60; i++ {
		owns = append(owns, fmt.Sprintf("hub/internal/store/some_reasonably_long_directory/file_number_%03d_test.go", i))
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wait-b", Operation: "wait_set", EntryID: b.ID, WaitOnEntryID: a.ID, WaitPaths: owns, WaitUntil: api.TeamQueueWaitAccepted}); err != nil {
		t.Fatal(err)
	}
	got := listedEntry(t, s, task.ID, b.ID)
	if got.Stall == nil || len(got.Waits) != 1 || len(got.Waits[0].Paths) != 60 {
		t.Fatalf("stall %v, waits %+v", got.Stall, got.Waits)
	}
	sentence := got.Waits[0].Text()
	if grown := len(got.BlockReason) - len(before.BlockReason); grown != len(". "+sentence) || len(sentence) > 600 || !strings.HasPrefix(got.BlockReason, before.BlockReason) {
		t.Fatalf("blockReason grew %d bytes for a %d byte sentence: %q", grown, len(sentence), got.BlockReason)
	}
	if !strings.HasPrefix(sentence, "waits on "+items[0].ID+" for "+owns[0]+", "+owns[1]) || !strings.HasSuffix(sentence, " more until accepted") || strings.Contains(sentence, owns[59]) {
		t.Fatalf("sentence %q", sentence)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: got.Stall.NoticeRequestID(b.ID), Operation: "stall_notice", EntryID: b.ID}); err != nil {
		t.Fatalf("stall notice of an entry with a 60 path wait: %v", err)
	}
	// The largest wait the store accepts still fits the notice body.
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "clear-b", Operation: "wait_clear", EntryID: b.ID, WaitOnEntryID: a.ID}); err != nil {
		t.Fatal(err)
	}
	owns = owns[:0]
	for i := 0; i < 256; i++ {
		owns = append(owns, fmt.Sprintf("%03d/%s", i, strings.Repeat("p", 1020)))
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "wait-b-max", Operation: "wait_set", EntryID: b.ID, WaitOnEntryID: a.ID, WaitPaths: owns, WaitUntil: api.TeamQueueWaitAccepted}); err != nil {
		t.Fatal(err)
	}
	if got = listedEntry(t, s, task.ID, b.ID); len(got.Waits) != 1 || len(got.Waits[0].Paths) != 256 || len(got.BlockReason) > len(before.BlockReason)+1200 || !strings.Contains(got.BlockReason, "and 255 more until accepted") {
		t.Fatalf("largest wait: %d paths, blockReason %d bytes", len(got.Waits[0].Paths), len(got.BlockReason))
	}
}

// Review f1: a queued waiting entry has no lead of its own. The project
// orchestrator, here another team's lead, is not told and the wait stays
// waiting; the entry's own lead is told once it exists.
func TestQueueWaitMetNeverFallsBackToProjectOrchestrator(t *testing.T) {
	f := newWaitFixture(t)
	ref := api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}
	other, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "pred-lead", AgentID: api.NewID("agt"), Host: "mini", Session: "pred-lead",
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.items[0].ID, ItemRevision: f.items[0].Revision, WorkOrderMessage: ref, ContextBundle: syntheticPreparedContext(t, f.items[0], ref, syntheticHistory(f.items[0], f.orders[0]))}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE tasks SET orchestrator='pred-lead' WHERE id=?`, f.task.ID)
	f.exec(t, `INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, f.task.ID, f.items[0].ID, other.ID, other.RunID)
	f.set(t, api.TeamQueueWaitAccepted)
	f.meet(t, api.TeamQueueWaitAccepted)
	before := f.messageCount(t)
	for i := 0; i < 3; i++ {
		if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitWaiting {
			t.Fatalf("queued entry without a lead: steps %v, state %s", steps, f.state(t))
		}
	}
	if f.messageCount(t) != before || len(f.notices(t, other, queueWaitMetSubject)) != 0 {
		t.Fatal("the project orchestrator was told of another entry's wait")
	}
	// A closed lead row, or a lead whose agent is closed, is no lead either.
	f.team(t)
	f.exec(t, `UPDATE item_team_leads SET state='closed' WHERE item_id=?`, f.items[1].ID)
	if steps := f.sweep(t); len(steps) != 0 {
		t.Fatalf("closed lead row: %v", steps)
	}
	f.exec(t, `UPDATE item_team_leads SET state='running' WHERE item_id=?`, f.items[1].ID)
	f.exec(t, `UPDATE agents SET status='closed' WHERE id=?`, f.lead.ID)
	if steps := f.sweep(t); len(steps) != 0 || f.state(t) != api.TeamQueueWaitWaiting {
		t.Fatalf("closed lead agent: %v", steps)
	}
	f.exec(t, `UPDATE agents SET status='done' WHERE id=?`, f.lead.ID)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
		t.Fatalf("with its own lead: %v", steps)
	}
	if own, orchestrator := f.notices(t, f.lead, queueWaitMetSubject), f.notices(t, other, queueWaitMetSubject); len(own) != 1 || len(orchestrator) != 0 {
		t.Fatalf("%d notices to the entry's lead, %d to the orchestrator", len(own), len(orchestrator))
	}
	if steps := f.sweep(t); len(steps) != 0 {
		t.Fatalf("told twice: %v", steps)
	}
}

// Review f2: a member waiting for or holding the matrix host is the team's
// open work, as in the stall check: the team is not reported as idle. A
// matrix wait the relay stopped updating counts only within its bound.
func TestQueueWaitMatrixWaitIsNotIdle(t *testing.T) {
	for name, c := range map[string]struct {
		wait  func(now time.Time) *api.MatrixWait
		state string
		steps int
	}{
		"waiting for the host": {func(now time.Time) *api.MatrixWait {
			return &api.MatrixWait{Role: api.MatrixWaitWaiting, Position: 1, Length: 1, Since: now}
		}, api.TeamQueueWaitResumed, 0},
		"holding the host": {func(now time.Time) *api.MatrixWait {
			return &api.MatrixWait{Role: api.MatrixWaitRunning, Since: now}
		}, api.TeamQueueWaitResumed, 0},
		"a wait past its bound": {func(now time.Time) *api.MatrixWait {
			return &api.MatrixWait{Role: api.MatrixWaitWaiting, Position: 1, Length: 1, Since: now.Add(-defaultQueueMatrixWaitBound)}
		}, api.TeamQueueWaitOverdue, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			helper := f.helper(t)
			f.set(t, api.TeamQueueWaitAccepted)
			f.meet(t, api.TeamQueueWaitAccepted)
			f.sweep(t)
			// Every member is done and idle; only the matrix run is open.
			if _, err := f.s.ReportActivity(f.ctx, f.task.ID, f.member.ID, api.ActivityReport{RequestID: api.NewID("req"), RunID: f.member.RunID, Activity: api.AgentActivity{State: "idle", ObservedAt: f.clock, MatrixWait: c.wait(f.clock)}}); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE agents SET status='done' WHERE id=?`, f.member.ID)
			f.clock = f.clock.Add(15 * time.Minute)
			if steps := f.sweep(t); len(steps) != c.steps || f.state(t) != c.state {
				t.Fatalf("steps %v, state %s, want %d and %s", steps, f.state(t), c.steps, c.state)
			}
			if n := len(f.notices(t, helper, queueWaitOverdueSubject)) + len(f.notices(t, f.handler, queueWaitOverdueSubject)); n != 2*c.steps {
				t.Fatalf("%d overdue notices", n)
			}
		})
	}
}

// wi_fffe53255abf1817 r1-r3: a met wait counts a member's matrix wait by the
// stall check's rule. A retired member's saved wait is not open work, and a
// since saved in the future before ReportActivity's clamp counts only to the
// bound from the report's observedAt.
func TestQueueWaitMatrixWaitRule(t *testing.T) {
	for name, c := range map[string]struct {
		status string
		// observed and since rewrite the saved row as one stored before the
		// clamp; a zero observed keeps the live report.
		observed, since time.Duration
		state           string
		steps           int
	}{
		"retired member's wait":         {status: api.AgentRetired, state: api.TeamQueueWaitOverdue, steps: 1},
		"live member's wait":            {status: api.AgentDone, state: api.TeamQueueWaitResumed},
		"future since past the bound":   {status: api.AgentDone, observed: -defaultQueueMatrixWaitBound, since: 10 * time.Hour, state: api.TeamQueueWaitOverdue, steps: 1},
		"future since inside the bound": {status: api.AgentDone, observed: -time.Minute, since: 10 * time.Hour, state: api.TeamQueueWaitResumed},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			helper := f.helper(t)
			f.set(t, api.TeamQueueWaitAccepted)
			f.meet(t, api.TeamQueueWaitAccepted)
			f.sweep(t)
			if _, err := f.s.ReportActivity(f.ctx, f.task.ID, f.member.ID, api.ActivityReport{RequestID: api.NewID("req"), RunID: f.member.RunID, Activity: api.AgentActivity{State: "idle", ObservedAt: f.clock,
				MatrixWait: &api.MatrixWait{Role: api.MatrixWaitWaiting, Position: 1, Length: 1, Since: f.clock}}}); err != nil {
				t.Fatal(err)
			}
			if c.observed != 0 {
				observed := f.clock.Add(c.observed)
				payload := fmt.Sprintf(`{"state":"idle","observedAt":%q,"matrixWait":{"role":"waiting","position":1,"length":1,"since":%q}}`, ts(observed), ts(f.clock.Add(c.since)))
				f.exec(t, `UPDATE agent_activity SET observed_at=?,payload=? WHERE agent_id=? AND run_id=?`, ts(observed), payload, f.member.ID, f.member.RunID)
			}
			f.exec(t, `UPDATE agents SET status=? WHERE id=?`, c.status, f.member.ID)
			f.clock = f.clock.Add(15 * time.Minute)
			if steps := f.sweep(t); len(steps) != c.steps || f.state(t) != c.state {
				t.Fatalf("steps %v, state %s, want %d and %s", steps, f.state(t), c.steps, c.state)
			}
			if n := len(f.notices(t, helper, queueWaitOverdueSubject)) + len(f.notices(t, f.handler, queueWaitOverdueSubject)); n != 2*c.steps {
				t.Fatalf("%d overdue notices", n)
			}
		})
	}
}

// A provider-blocked member makes no progress, so it counts as idle, as in
// the stall check's member rule; its matrix run still keeps the team open.
func TestQueueWaitProviderBlockedMemberIsIdle(t *testing.T) {
	for name, c := range map[string]struct {
		wait  func(now time.Time) *api.MatrixWait
		state string
		steps int
	}{
		"blocked": {func(time.Time) *api.MatrixWait { return nil }, api.TeamQueueWaitOverdue, 1},
		"blocked holding the matrix host": {func(now time.Time) *api.MatrixWait {
			return &api.MatrixWait{Role: api.MatrixWaitRunning, Since: now}
		}, api.TeamQueueWaitResumed, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			helper := f.helper(t)
			f.set(t, api.TeamQueueWaitAccepted)
			f.meet(t, api.TeamQueueWaitAccepted)
			f.sweep(t)
			runtime := f.member.Runtime
			if runtime != "codex" {
				runtime = "claude"
			}
			// A done member is idle already, so the blocked member is running.
			activity := api.AgentActivity{State: "provider_blocked", ObservedAt: f.clock, Provider: providerBlock(runtime, api.ProviderBlockUsageLimit, f.clock), MatrixWait: c.wait(f.clock)}
			if _, err := f.s.ReportActivity(f.ctx, f.task.ID, f.member.ID, api.ActivityReport{RequestID: api.NewID("req"), RunID: f.member.RunID, Activity: activity}); err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE agents SET status='running' WHERE id=?`, f.member.ID)
			f.clock = f.clock.Add(15 * time.Minute)
			if steps := f.sweep(t); len(steps) != c.steps || f.state(t) != c.state {
				t.Fatalf("steps %v, state %s, want %d and %s", steps, f.state(t), c.steps, c.state)
			}
			for _, to := range []api.Agent{helper, f.handler} {
				if n := len(f.notices(t, to, queueWaitOverdueSubject)); n != c.steps {
					t.Fatalf("%d overdue notices to %s", n, to.Name)
				}
			}
		})
	}
}

// Review f3: a tick where a wait cannot step takes no write lock. The test
// holds the store's write lock, so a sweep that needed it would not return.
func TestQueueWaitSweepTakesNoWriteLockWhenNoOneCanBeTold(t *testing.T) {
	for name, arrange := range map[string]func(*testing.T, *waitFixture){
		"met with no lead yet": func(t *testing.T, f *waitFixture) {
			f.set(t, api.TeamQueueWaitAccepted)
			f.meet(t, api.TeamQueueWaitAccepted)
		},
		"orphaned with no primary handler": func(t *testing.T, f *waitFixture) {
			f.team(t)
			f.set(t, api.TeamQueueWaitAccepted)
			f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
			f.exec(t, `UPDATE agents SET status='closed' WHERE task_id=? AND role=?`, f.task.ID, api.AgentRoleDatabaseHandler)
		},
		"orphaned with a retired primary handler": func(t *testing.T, f *waitFixture) {
			f.team(t)
			f.set(t, api.TeamQueueWaitAccepted)
			f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
			f.exec(t, `UPDATE agents SET status='retired' WHERE task_id=? AND role=?`, f.task.ID, api.AgentRoleDatabaseHandler)
		},
		"waiting on a pending predecessor": func(t *testing.T, f *waitFixture) {
			f.team(t)
			f.set(t, api.TeamQueueWaitReleased)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWaitFixture(t)
			arrange(t, f)
			before := f.messageCount(t)
			f.s.writeMu.Lock()
			done := make(chan error, 1)
			go func() {
				steps, err := f.s.QueueWaitSweep(f.ctx, f.clock)
				if err == nil && len(steps) != 0 {
					err = fmt.Errorf("steps %v", steps)
				}
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				err = errors.New("the sweep waited for the write lock")
			}
			f.s.writeMu.Unlock()
			if err != nil {
				<-done
				t.Fatal(err)
			}
			if f.state(t) != api.TeamQueueWaitWaiting || f.messageCount(t) != before {
				t.Fatalf("state %s, %d new messages", f.state(t), f.messageCount(t)-before)
			}
		})
	}
	// With someone to tell, the same waits step on the next sweep.
	f := newWaitFixture(t)
	f.team(t)
	f.set(t, api.TeamQueueWaitAccepted)
	f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
	f.exec(t, `UPDATE agents SET status='closed' WHERE task_id=? AND role=?`, f.task.ID, api.AgentRoleDatabaseHandler)
	if steps := f.sweep(t); len(steps) != 0 {
		t.Fatalf("no handler: %v", steps)
	}
	f.exec(t, `UPDATE agents SET status='running' WHERE id=?`, f.handler.ID)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-orphaned" {
		t.Fatalf("handler back: %v", steps)
	}
}

// w1: a retired member's saved matrix wait is not open work, so its idle team
// stalls as idle-entry once the idle threshold passes, well inside the
// wait's bound. A live member's wait still keeps the team open.
func TestQueueStallRetiredMemberMatrixWait(t *testing.T) {
	for status, stalls := range map[string]bool{api.AgentRetired: true, api.AgentRunning: false, api.AgentDone: false} {
		t.Run(status, func(t *testing.T) {
			f, advance, a, b, lead, _ := idleTeam(t)
			if _, err := f.report(t, lead, &api.MatrixWait{Role: api.MatrixWaitWaiting, Item: f.items[0].ID, Position: 1, Length: 1, Since: f.s.now()}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, status, lead.ID); err != nil {
				t.Fatal(err)
			}
			advance(40 * time.Minute)
			if !stalls {
				assertNoStall(t, f, b.ID, "a live member waits for the matrix host")
				advance(defaultQueueMatrixWaitBound - 41*time.Minute)
				assertNoStall(t, f, b.ID, "a live member's wait inside its bound")
				return
			}
			assertStall(t, f, b.ID, api.StallIdleEntry, a.ID, "tt team queue scope --task "+f.task.ID+" --entry "+a.ID)
		})
	}
}

// w2: a matrix wait since more than a minute after the report's observedAt
// is saved as observedAt, and a repeat of the request returns that saved
// receipt; a since within the minute is saved as sent.
func TestActivityMatrixWaitFutureSinceClamped(t *testing.T) {
	f, _, _, _, lead, _ := idleTeam(t)
	now := f.s.now()
	report := func(key string, since time.Time) api.AgentActivity {
		t.Helper()
		saved, err := f.s.ReportActivity(context.Background(), f.task.ID, lead.ID, api.ActivityReport{RequestID: key, RunID: lead.RunID, Activity: api.AgentActivity{State: "idle", ObservedAt: now, MatrixWait: &api.MatrixWait{Role: api.MatrixWaitRunning, Since: since}}})
		if err != nil {
			t.Fatal(err)
		}
		var payload string
		if err := f.s.db.QueryRow(`SELECT payload FROM agent_activity WHERE agent_id=? AND run_id=?`, lead.ID, lead.RunID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var stored api.AgentActivity
		if err := json.Unmarshal([]byte(payload), &stored); err != nil {
			t.Fatal(err)
		}
		if !api.SameMatrixWait(saved.MatrixWait, stored.MatrixWait) {
			t.Fatalf("returned %+v, stored %+v", saved.MatrixWait, stored.MatrixWait)
		}
		return saved
	}
	if got := report("future-since", now.Add(2*time.Hour)); !got.MatrixWait.Since.Equal(now) {
		t.Fatalf("future since saved as %s, want %s", got.MatrixWait.Since, now)
	}
	if got := report("future-since", now.Add(2*time.Hour)); !got.MatrixWait.Since.Equal(now) {
		t.Fatalf("repeat returned since %s, want the saved %s", got.MatrixWait.Since, now)
	}
	if got := report("skewed-since", now.Add(time.Minute)); !got.MatrixWait.Since.Equal(now.Add(time.Minute)) {
		t.Fatalf("since within the skew saved as %s", got.MatrixWait.Since)
	}
}

// w2: a future since cannot outlive the bound: one clamped on save, and one
// saved before the clamp, both stop counting the bound after observedAt.
func TestQueueStallFutureMatrixWaitSinceBounded(t *testing.T) {
	for name, save := range map[string]func(*choresQueue, api.Agent) error{
		"clamped on save": func(f *choresQueue, lead api.Agent) error {
			_, err := f.report(t, lead, &api.MatrixWait{Role: api.MatrixWaitRunning, Since: f.s.now().Add(2 * time.Hour)})
			return err
		},
		"saved before the clamp": func(f *choresQueue, lead api.Agent) error {
			payload := fmt.Sprintf(`{"state":"idle","observedAt":%q,"matrixWait":{"role":"running","since":%q}}`, ts(f.s.now()), ts(f.s.now().Add(10*time.Hour)))
			_, err := f.s.db.Exec(`UPDATE agent_activity SET observed_at=?,payload=? WHERE agent_id=? AND run_id=?`, ts(f.s.now()), payload, lead.ID, lead.RunID)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, advance, a, b, lead, _ := idleTeam(t)
			if err := save(f, lead); err != nil {
				t.Fatal(err)
			}
			advance(defaultQueueMatrixWaitBound - time.Minute)
			assertNoStall(t, f, b.ID, "a wait inside the bound from its report")
			advance(2 * time.Minute)
			assertStall(t, f, b.ID, api.StallIdleEntry, a.ID)
		})
	}
}

// waitPause is a pathless wait's reason: a requested pause that shares no path.
const waitPause = "a quiet host while the predecessor's release runs go-test"

// wi_b787a95c1757f554 h1: the owner or a live handler records a wait that
// names a reason instead of paths. Neither, both, a long or multi-line
// reason, and a lead are refused and store nothing; an identical repeat
// changes nothing and another reason on the same pair is a conflict.
func TestTeamQueueWaitPathlessSetAndRefusals(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	for name, c := range map[string]struct {
		edit func(*api.TeamQueueRequest)
		kind error
		text string
	}{
		"neither a path nor a reason": {func(r *api.TeamQueueRequest) { r.WaitReason = "" }, api.ErrInvalid, "a wait needs at least one path, or a reason for a pause that shares no path"},
		"a reason of only spaces":     {func(r *api.TeamQueueRequest) { r.WaitReason = "   " }, api.ErrInvalid, "at least one path, or a reason"},
		"both paths and a reason":     {func(r *api.TeamQueueRequest) { r.WaitPaths = []string{waitPath} }, api.ErrInvalid, "a wait names shared paths or a reason, not both"},
		"a reason over 500 bytes":     {func(r *api.TeamQueueRequest) { r.WaitReason = strings.Repeat("x", 501) }, api.ErrInvalid, "a wait reason is one line of at most 500 bytes"},
		"a reason of two lines":       {func(r *api.TeamQueueRequest) { r.WaitReason = "quiet host\nfor a release" }, api.ErrInvalid, "one line"},
		"an item lead":                {func(r *api.TeamQueueRequest) { r.LeadAgentID, r.LeadRunID = f.lead.ID, f.lead.RunID }, api.ErrConflict, "only the owner or a database handler"},
	} {
		req := f.setPause(api.TeamQueueWaitReleased, waitPause)
		c.edit(&req)
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req); !errors.Is(err, c.kind) || !strings.Contains(err.Error(), c.text) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var rows int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_waits`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused waits stored %d rows %v", rows, err)
	}

	req := f.setPause(api.TeamQueueWaitReleased, "  "+waitPause+" ")
	req.HandlerAgentID, req.HandlerRunID = f.handler.ID, f.handler.RunID
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil || len(q.Waits) != 1 {
		t.Fatalf("handler pathless set %+v %v", q, err)
	}
	w := q.Waits[0]
	if w.Paths == nil || len(w.Paths) != 0 || w.Reason != waitPause || w.Until != api.TeamQueueWaitReleased || w.State != api.TeamQueueWaitWaiting || w.OnItemID != f.items[0].ID {
		t.Fatalf("wait %+v", w)
	}
	var pathsJSON, reason, by, setAt string
	if err := f.s.db.QueryRow(`SELECT paths_json,reason,set_by,set_at FROM team_queue_waits`).Scan(&pathsJSON, &reason, &by, &setAt); err != nil || pathsJSON != "[]" || reason != waitPause || by != f.handler.ID {
		t.Fatalf("row %q %q %q %v", pathsJSON, reason, by, err)
	}
	sentence := fmt.Sprintf("waits on %s for %s until released", f.items[0].ID, waitPause)
	if listed := f.waitingEntry(t); len(listed.Waits) != 1 || listed.Waits[0].Text() != sentence || !strings.HasSuffix(listed.BlockReason, sentence) || listed.Revision != f.waiting.Revision {
		t.Fatalf("listed %+v reason %q", listed.Waits, listed.BlockReason)
	}
	f.clock = f.clock.Add(time.Minute)
	if again, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitReleased, waitPause)); err != nil || len(again.Waits) != 1 || again.Waits[0].SetAt != setAt {
		t.Fatalf("identical repeat changed the wait: %+v %v", again.Waits, err)
	}
	clearCommand := fmt.Sprintf("tt team queue wait clear --task %s --entry %s --on %s", f.task.ID, f.waiting.ID, f.pred.ID)
	for name, other := range map[string]api.TeamQueueRequest{
		"another reason":    f.setPause(api.TeamQueueWaitReleased, "another pause"),
		"another condition": f.setPause(api.TeamQueueWaitAccepted, waitPause),
		"paths instead":     f.setRequest(api.TeamQueueWaitReleased),
	} {
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, other); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), clearCommand) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// wi_b787a95c1757f554 h2, h3: a pathless wait on a predecessor's release
// tells the waiting lead exactly once when the release job is released,
// naming the reason where a path wait names its paths.
func TestTeamQueueWaitPathlessReleasedNotifiesOnce(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitReleased, waitPause)); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'verified',1,'{}')`, f.task.ID, api.NewID("rel"), f.pred.ID)
	before := f.messageCount(t)
	if steps := f.sweep(t); len(steps) != 0 || f.messageCount(t) != before {
		t.Fatalf("steps while the release runs: %v", steps)
	}
	f.clock = f.clock.Add(time.Minute)
	f.meet(t, api.TeamQueueWaitReleased)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
		t.Fatalf("steps at the release: %v", steps)
	}
	got := f.notices(t, f.lead, queueWaitMetSubject)
	if len(got) != 1 {
		t.Fatalf("%d notices to the lead", len(got))
	}
	text, refs := got[0].Envelope.Body.Text, got[0].Envelope.Refs
	for _, want := range []string{f.items[0].ID, f.pred.ID, "until it was released", "released commit " + waitReleased, "Paused for: " + waitPause + ".", "Resume the paused work."} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "Paths now free") || strings.Contains(text, "these paths") {
		t.Fatalf("a pathless notice names paths: %s", text)
	}
	if refs["activity"] != "queue_wait" || refs["condition"] != api.TeamQueueWaitReleased || refs["commit"] != waitReleased || refs["predecessorEntry"] != f.pred.ID {
		t.Fatalf("refs %v", refs)
	}
	sentence := fmt.Sprintf("wait on %s met (released, commit %s): resume the work paused for %s", f.items[0].ID, waitReleased, waitPause)
	if listed := f.waitingEntry(t); len(listed.Waits) != 1 || listed.Waits[0].State != api.TeamQueueWaitMet || listed.Waits[0].Text() != sentence || !strings.HasSuffix(listed.BlockReason, sentence) {
		t.Fatalf("listed %+v %q", listed.Waits, listed.BlockReason)
	}
	for i := 0; i < 3; i++ {
		f.clock = f.clock.Add(30 * time.Second)
		if steps := f.sweep(t); len(steps) != 0 {
			t.Fatalf("sweep %d repeated: %v", i, steps)
		}
	}
	f.reopen(t)
	f.clock = f.clock.Add(30 * time.Second)
	if steps := f.sweep(t); len(steps) != 0 {
		t.Fatalf("sweep after reopen repeated: %v", steps)
	}
	if got := f.notices(t, f.lead, queueWaitMetSubject); len(got) != 1 {
		t.Fatalf("%d notices to the lead after more sweeps and a reopen", len(got))
	}
}

// wi_b787a95c1757f554 h2: a met pathless wait whose team stays idle is
// reported once to the owner helper and the handler, and a pathless wait
// whose predecessor can no longer meet it is reported once to the handler,
// each naming the reason.
func TestTeamQueueWaitPathlessOverdueAndOrphaned(t *testing.T) {
	t.Run("overdue", func(t *testing.T) {
		f := newWaitFixture(t)
		f.team(t)
		helper := f.helper(t)
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitReleased, waitPause)); err != nil {
			t.Fatal(err)
		}
		f.meet(t, api.TeamQueueWaitReleased)
		f.sweep(t)
		f.clock = f.clock.Add(queueWaitIdleBound())
		if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-overdue" {
			t.Fatalf("at the bound: %v", steps)
		}
		for _, to := range []api.Agent{helper, f.handler} {
			got := f.notices(t, to, queueWaitOverdueSubject)
			if len(got) != 1 {
				t.Fatalf("%d overdue notices to %s", len(got), to.Name)
			}
			text := got[0].Envelope.Body.Text
			if !strings.Contains(text, "Paused for: "+waitPause+".") || strings.Contains(text, "Paths free") || !strings.Contains(text, f.waiting.ID) {
				t.Fatalf("overdue notice %s", text)
			}
		}
		for i := 0; i < 3; i++ {
			f.clock = f.clock.Add(time.Minute)
			if steps := f.sweep(t); len(steps) != 0 {
				t.Fatalf("sweep %d reported again: %v", i, steps)
			}
		}
	})
	t.Run("orphaned", func(t *testing.T) {
		f := newWaitFixture(t)
		f.team(t)
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitReleased, waitPause)); err != nil {
			t.Fatal(err)
		}
		f.exec(t, `UPDATE team_queue_entries SET state='finished' WHERE id=?`, f.pred.ID)
		if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-orphaned" {
			t.Fatalf("steps %v", steps)
		}
		got := f.notices(t, f.handler, queueWaitOrphanedSubject)
		if len(got) != 1 {
			t.Fatalf("%d notices to the handler", len(got))
		}
		text := got[0].Envelope.Body.Text
		for _, want := range []string{"for " + waitPause + " until it is released", "the predecessor entry finished without a release job", "--owns PATH|--reason TEXT --until"} {
			if !strings.Contains(text, want) {
				t.Fatalf("handler notice lacks %q: %s", want, text)
			}
		}
		if got := f.notices(t, f.lead, queueWaitMetSubject); len(got) != 0 {
			t.Fatalf("the lead was told of an orphaned wait")
		}
		sentence := fmt.Sprintf("wait on %s needs a handler decision: the predecessor entry finished without a release job", f.items[0].ID)
		if listed := f.waitingEntry(t); len(listed.Waits) != 1 || listed.Waits[0].Text() != sentence {
			t.Fatalf("listed %+v", listed.Waits)
		}
		f.clock = f.clock.Add(time.Minute)
		if steps := f.sweep(t); len(steps) != 0 {
			t.Fatalf("reported again: %v", steps)
		}
	})
}

// wi_b787a95c1757f554 h3: a pathless wait clears like a path wait, and the
// pair can then be set again with another reason or with paths.
func TestTeamQueueWaitPathlessSetClearRoundTrip(t *testing.T) {
	f := newWaitFixture(t)
	if q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitDone, waitPause)); err != nil || len(q.Waits) != 1 {
		t.Fatalf("set %+v %v", q, err)
	}
	if q := f.clear(t); len(q.Waits) != 0 {
		t.Fatalf("clear result still waits: %+v", q.Waits)
	}
	if listed := f.waitingEntry(t); len(listed.Waits) != 0 || strings.Contains(listed.BlockReason, "waits on") {
		t.Fatalf("cleared wait still listed: %+v %q", listed.Waits, listed.BlockReason)
	}
	var state, reason string
	if err := f.s.db.QueryRow(`SELECT state,reason FROM team_queue_waits`).Scan(&state, &reason); err != nil || state != api.TeamQueueWaitCleared || reason != "sequencing changed" {
		t.Fatalf("cleared row %s %q %v", state, reason, err)
	}
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitAccepted, "another pause"))
	if err != nil || len(q.Waits) != 1 || q.Waits[0].Reason != "another pause" || q.Waits[0].Until != api.TeamQueueWaitAccepted {
		t.Fatalf("set after clear %+v %v", q.Waits, err)
	}
	f.clear(t)
	if q := f.set(t, api.TeamQueueWaitAccepted); len(q.Waits) != 1 || len(q.Waits[0].Paths) != 1 || q.Waits[0].Reason != "" {
		t.Fatalf("path wait after a pathless one: %+v", q.Waits)
	}
}

// wi_b787a95c1757f554 h2: a 500-byte reason is cut in the listing sentence
// to the path list's bound on a rune boundary, so BlockReason stays small;
// the wait's Reason keeps the whole text.
func TestTeamQueueWaitPathlessSentenceIsBounded(t *testing.T) {
	f := newWaitFixture(t)
	long := "a" + strings.Repeat("é", 249) + "a"
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitAccepted, long)); err != nil {
		t.Fatal(err)
	}
	listed := f.waitingEntry(t)
	if len(listed.Waits) != 1 || listed.Waits[0].Reason != long {
		t.Fatalf("listed %+v", listed.Waits)
	}
	text := listed.Waits[0].Text()
	if len(text) > 600 || !utf8.ValidString(text) || !strings.HasSuffix(text, "... until accepted") || !strings.HasSuffix(listed.BlockReason, text) {
		t.Fatalf("sentence %d bytes: %q", len(text), text)
	}
}

// waitGate is a handler-gated pause reason like incident I-50483's: a
// physical host wait whose start needs the handler's explicit resume.
const waitGate = "matrix host held by another run; start only on the database handler's explicit resume after its host, ledger and plan checks"

// setGated is a wait with resume handler: pathless with a reason, or on paths.
func (f *waitFixture) setGated(t *testing.T, until, reason string, paths ...string) api.TeamQueueEntry {
	t.Helper()
	req := f.setPause(until, reason)
	if len(paths) > 0 {
		req = f.setRequest(until, paths...)
	}
	req.WaitResume = api.TeamQueueWaitResumeHandler
	req.HandlerAgentID, req.HandlerRunID = f.handler.ID, f.handler.RunID
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil {
		t.Fatalf("gated wait set: %v", err)
	}
	return q
}

func (f *waitFixture) resume(t *testing.T) string {
	t.Helper()
	var resume string
	if err := f.s.db.QueryRow(`SELECT resume FROM team_queue_waits WHERE task_id=? AND entry_id=? AND on_entry_id=?`, f.task.ID, f.waiting.ID, f.pred.ID).Scan(&resume); err != nil {
		t.Fatal(err)
	}
	return resume
}

// wi_99ce2fa485c23648 a1: resume handler is a typed field of the wait. The
// owner or a live handler stores it and the listing shows it; an omitted
// resume is automatic and left out of the JSON. Any other value, a resume on
// clear and an item lead are refused; an identical repeat changes nothing
// and a different resume on the same pair is a conflict naming wait clear.
func TestQueueWaitHandlerResumeSetAndRefusals(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	for name, c := range map[string]struct {
		edit func(*api.TeamQueueRequest)
		kind error
		text string
	}{
		"an unknown resume": {func(r *api.TeamQueueRequest) { r.WaitResume = "manual" }, api.ErrInvalid, "a wait's resume is handler, or omitted for an automatic resume"},
		"a resume in caps":  {func(r *api.TeamQueueRequest) { r.WaitResume = "Handler" }, api.ErrInvalid, "a wait's resume is handler"},
		"an item lead":      {func(r *api.TeamQueueRequest) { r.LeadAgentID, r.LeadRunID = f.lead.ID, f.lead.RunID }, api.ErrConflict, "only the owner or a database handler"},
	} {
		req := f.setPause(api.TeamQueueWaitAccepted, waitGate)
		req.WaitResume = api.TeamQueueWaitResumeHandler
		c.edit(&req)
		if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req); !errors.Is(err, c.kind) || !strings.Contains(err.Error(), c.text) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var rows int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_waits`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused waits stored %d rows %v", rows, err)
	}

	q := f.setGated(t, api.TeamQueueWaitAccepted, waitGate)
	if len(q.Waits) != 1 || q.Waits[0].Resume != api.TeamQueueWaitResumeHandler || q.Waits[0].Reason != waitGate {
		t.Fatalf("gated set %+v", q.Waits)
	}
	var by, setAt string
	if err := f.s.db.QueryRow(`SELECT set_by,set_at FROM team_queue_waits`).Scan(&by, &setAt); err != nil || by != f.handler.ID || f.resume(t) != api.TeamQueueWaitResumeHandler {
		t.Fatalf("row by %q resume %q %v", by, f.resume(t), err)
	}
	listed := f.waitingEntry(t)
	encoded, _ := json.Marshal(listed.Waits)
	sentence := fmt.Sprintf("waits on %s for %s until accepted, then a handler resume", f.items[0].ID, waitGate)
	if len(listed.Waits) != 1 || listed.Waits[0].Resume != api.TeamQueueWaitResumeHandler || !strings.Contains(string(encoded), `"resume":"handler"`) ||
		listed.Waits[0].Text() != sentence || !strings.HasSuffix(listed.BlockReason, sentence) {
		t.Fatalf("listed %s %q", encoded, listed.BlockReason)
	}

	// The owner repeats the identical wait: nothing changes.
	f.clock = f.clock.Add(time.Minute)
	same := f.setPause(api.TeamQueueWaitAccepted, waitGate)
	same.WaitResume = api.TeamQueueWaitResumeHandler
	if again, err := f.s.TeamQueueAction(f.ctx, f.task.ID, same); err != nil || len(again.Waits) != 1 || again.Waits[0].SetAt != setAt || again.Waits[0].Resume != api.TeamQueueWaitResumeHandler {
		t.Fatalf("identical repeat changed the wait: %+v %v", again.Waits, err)
	}
	// The same pair as an automatic wait is a different wait.
	clearCommand := fmt.Sprintf("tt team queue wait clear --task %s --entry %s --on %s", f.task.ID, f.waiting.ID, f.pred.ID)
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitAccepted, waitGate)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), clearCommand) {
		t.Fatalf("an automatic repeat of a gated wait: %v", err)
	}
	if f.resume(t) != api.TeamQueueWaitResumeHandler {
		t.Fatalf("a refused repeat changed the resume to %q", f.resume(t))
	}
	// wait clear takes no resume.
	clearResume := api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "wait_clear", EntryID: f.waiting.ID, WaitOnEntryID: f.pred.ID, WaitResume: api.TeamQueueWaitResumeHandler}
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, clearResume); !errors.Is(err, api.ErrInvalid) || !strings.Contains(err.Error(), "no resume") {
		t.Fatalf("clear with a resume: %v", err)
	}
	if f.state(t) != api.TeamQueueWaitWaiting {
		t.Fatalf("a refused clear changed the state to %s", f.state(t))
	}
	f.clear(t)

	// An omitted resume is automatic: stored empty and left out of the JSON.
	if q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.setPause(api.TeamQueueWaitAccepted, waitGate)); err != nil || len(q.Waits) != 1 || q.Waits[0].Resume != "" {
		t.Fatalf("automatic set %+v %v", q, err)
	}
	listed = f.waitingEntry(t)
	encoded, _ = json.Marshal(listed.Waits)
	if f.resume(t) != "" || strings.Contains(string(encoded), `"resume"`) || listed.Waits[0].Text() != fmt.Sprintf("waits on %s for %s until accepted", f.items[0].ID, waitGate) {
		t.Fatalf("automatic wait %q %s", f.resume(t), encoded)
	}
	// The same pair as a gated wait is again a conflict.
	gated := f.setPause(api.TeamQueueWaitAccepted, waitGate)
	gated.WaitResume = api.TeamQueueWaitResumeHandler
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, gated); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), clearCommand) {
		t.Fatalf("a gated repeat of an automatic wait: %v", err)
	}
}

// wi_99ce2fa485c23648 a2, incident I-50483 reproduced: a pathless wait until
// accepted whose team must wait for the handler's explicit resume. Once the
// predecessor is accepted the lead alone gets exactly one notice that names
// the condition, the whole reason and the outstanding handler resume, and
// never says to resume the paused work. More sweeps and a reopen post nothing.
func TestQueueWaitHandlerResumeMetNamesTheGate(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	f.setGated(t, api.TeamQueueWaitAccepted, waitGate)
	before := f.messageCount(t)
	if steps := f.sweep(t); len(steps) != 0 || f.messageCount(t) != before {
		t.Fatalf("steps before acceptance: %v", steps)
	}
	f.clock = f.clock.Add(time.Minute)
	f.meet(t, api.TeamQueueWaitAccepted)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
		t.Fatalf("steps at acceptance: %v", steps)
	}
	if posted := f.messageCount(t) - before; posted != 1 {
		t.Fatalf("%d messages posted at acceptance", posted)
	}
	if got := f.notices(t, f.lead, queueWaitMetSubject); len(got) != 0 {
		t.Fatalf("a gated wait sent the automatic notice: %s", got[0].Envelope.Body.Text)
	}
	got := f.notices(t, f.lead, queueWaitGatedSubject)
	if len(got) != 1 {
		t.Fatalf("%d gated notices to the lead", len(got))
	}
	text, refs := got[0].Envelope.Body.Text, got[0].Envelope.Refs
	for _, want := range []string{f.waiting.ID, f.items[1].ID, f.items[0].ID, f.pred.ID, "until it was accepted", "That condition is now met", "accepted commit " + waitAccepted,
		"Paused for: " + waitGate + ".", "needs an explicit resume from the database handler", "do not start on this notice"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q: %s", want, text)
		}
	}
	for _, unwanted := range []string{"Resume the paused work", queueWaitMetSubject, "Paths now free"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("notice says %q: %s", unwanted, text)
		}
	}
	if refs["resume"] != api.TeamQueueWaitResumeHandler || refs["activity"] != "queue_wait" || refs["condition"] != api.TeamQueueWaitAccepted || refs["commit"] != waitAccepted ||
		refs["entry"] != f.waiting.ID || refs["predecessorEntry"] != f.pred.ID {
		t.Fatalf("refs %v", refs)
	}
	sentence := fmt.Sprintf("wait on %s met (accepted, commit %s): still paused for %s until the database handler resumes it", f.items[0].ID, waitAccepted, waitGate)
	listed := f.waitingEntry(t)
	if len(listed.Waits) != 1 || listed.Waits[0].State != api.TeamQueueWaitMet || listed.Waits[0].NoticeSeq != got[0].Seq || listed.Waits[0].Resume != api.TeamQueueWaitResumeHandler ||
		listed.Waits[0].Text() != sentence || !strings.HasSuffix(listed.BlockReason, sentence) || strings.Contains(listed.BlockReason, "resume the work paused") {
		t.Fatalf("listed %+v %q", listed.Waits, listed.BlockReason)
	}
	after := f.messageCount(t)
	for i := 0; i < 3; i++ {
		f.clock = f.clock.Add(30 * time.Second)
		if steps := f.sweep(t); len(steps) != 0 {
			t.Fatalf("sweep %d repeated: %v", i, steps)
		}
	}
	f.reopen(t)
	f.clock = f.clock.Add(30 * time.Second)
	if steps := f.sweep(t); len(steps) != 0 {
		t.Fatalf("sweep after reopen repeated: %v", steps)
	}
	if got := f.notices(t, f.lead, queueWaitGatedSubject); len(got) != 1 || f.messageCount(t) != after {
		t.Fatalf("%d gated notices and %d more messages after sweeps and a reopen", len(got), f.messageCount(t)-after)
	}
}

// wi_99ce2fa485c23648 a2: a gated wait on paths gets one notice naming the
// free paths and the handler resume, with the rebase only after that resume,
// and it fits the envelope with many paths.
func TestQueueWaitHandlerResumePathWaitNamesTheGate(t *testing.T) {
	var many []string
	for i := 0; i < 200; i++ {
		many = append(many, fmt.Sprintf("hub/internal/store/a_rather_long_generated_file_name_%03d_test.go", i))
	}
	for _, c := range []struct {
		name, until, shown, then string
		paths                    []string
	}{
		{"released", api.TeamQueueWaitReleased, "released commit " + waitReleased, "Once the handler resumes this team, rebase onto that commit before editing these paths.", []string{waitPath}},
		{"done without a commit", api.TeamQueueWaitDone, "Commit: none recorded", "Once the handler resumes this team, rebase onto the integrated branch before editing these paths.", []string{waitPath}},
		{"many paths", api.TeamQueueWaitAccepted, "accepted commit " + waitAccepted, "Once the handler resumes this team, rebase onto that commit before editing these paths.", many},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newWaitFixture(t)
			f.team(t)
			f.setGated(t, c.until, "", c.paths...)
			f.meet(t, c.until)
			if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
				t.Fatalf("steps %v", steps)
			}
			got := f.notices(t, f.lead, queueWaitGatedSubject)
			if len(got) != 1 || len(f.notices(t, f.lead, queueWaitMetSubject)) != 0 {
				t.Fatalf("%d gated notices", len(got))
			}
			text, refs := got[0].Envelope.Body.Text, got[0].Envelope.Refs
			for _, want := range []string{c.shown, "Paths now free: " + c.paths[0], "needs an explicit resume from the database handler: do not start on this notice", c.then} {
				if !strings.Contains(text, want) {
					t.Fatalf("notice lacks %q: %s", want, text)
				}
			}
			// No unconditional rebase: the only rebase follows the resume.
			if strings.Count(text, "ebase") != 1 || strings.Contains(text, "Resume the paused work") || len(text) > api.MaxEnvelopeBodyBytes || refs["resume"] != api.TeamQueueWaitResumeHandler {
				t.Fatalf("notice %d bytes %v: %s", len(text), refs, text)
			}
			listed := f.waitingEntry(t)
			if len(listed.Waits) != 1 || !strings.Contains(listed.Waits[0].Text(), "): the database handler resumes the team before it edits "+c.paths[0]) || strings.Contains(listed.BlockReason, "rebase before editing") {
				t.Fatalf("listed %+v %q", listed.Waits, listed.BlockReason)
			}
			for i := 0; i < 3; i++ {
				f.clock = f.clock.Add(30 * time.Second)
				if steps := f.sweep(t); len(steps) != 0 {
					t.Fatalf("sweep %d repeated: %v", i, steps)
				}
			}
		})
	}
}

// wi_99ce2fa485c23648 a3: a gated met wait whose team stays idle is reported
// once to the owner helper and once to the handler, naming the handler
// resume still owed rather than asking them to wake the lead. The met record
// holds the condition only.
func TestQueueWaitHandlerResumeOverdueNamesTheGate(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	helper := f.helper(t)
	f.setGated(t, api.TeamQueueWaitAccepted, waitGate)
	f.meet(t, api.TeamQueueWaitAccepted)
	if steps := f.sweep(t); len(steps) != 1 {
		t.Fatalf("steps at acceptance: %v", steps)
	}
	var metAt, metCommit string
	if err := f.s.db.QueryRow(`SELECT met_at,met_commit FROM team_queue_waits`).Scan(&metAt, &metCommit); err != nil || metAt != ts(f.clock) || metCommit != waitAccepted || f.state(t) != api.TeamQueueWaitMet {
		t.Fatalf("met record %q %q %s %v", metAt, metCommit, f.state(t), err)
	}
	f.clock = f.clock.Add(queueWaitIdleBound())
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-overdue" {
		t.Fatalf("at the bound: %v", steps)
	}
	notice := f.notices(t, f.lead, queueWaitGatedSubject)[0]
	for _, to := range []api.Agent{helper, f.handler} {
		got := f.notices(t, to, queueWaitOverdueSubject)
		if len(got) != 1 {
			t.Fatalf("%d overdue notices to %s", len(got), to.Name)
		}
		text, refs := got[0].Envelope.Body.Text, got[0].Envelope.Refs
		for _, want := range []string{f.waiting.ID, "Paused for: " + waitGate + ".", "this wait needs the database handler's explicit resume", "send the resume to the waiting lead, or clear the wait", "Nothing was restarted or signalled."} {
			if !strings.Contains(text, want) {
				t.Fatalf("overdue notice lacks %q: %s", want, text)
			}
		}
		if strings.Contains(text, "wake or message the waiting lead") || refs["resume"] != api.TeamQueueWaitResumeHandler || refs["notice"] != fmt.Sprint(notice.Seq) {
			t.Fatalf("overdue notice %v: %s", refs, text)
		}
	}
	if got := f.notices(t, f.lead, queueWaitOverdueSubject); len(got) != 0 {
		t.Fatalf("the waiting lead got the overdue report")
	}
	for i := 0; i < 3; i++ {
		f.clock = f.clock.Add(time.Minute)
		if steps := f.sweep(t); len(steps) != 0 {
			t.Fatalf("sweep %d reported again: %v", i, steps)
		}
	}
}

// wi_99ce2fa485c23648 a4: a store whose wait table predates resume gains the
// column with an empty default on open, its waiting row is swept as an
// automatic wait, and a second open changes nothing.
func TestQueueWaitResumeColumnMigrates(t *testing.T) {
	f := newWaitFixture(t)
	f.team(t)
	old := strings.Replace(teamQueueWaitsSQL, "\n resume TEXT NOT NULL DEFAULT '' CHECK(resume IN ('','handler')),", "", 1)
	if old == teamQueueWaitsSQL {
		t.Fatal("the old table text still has resume")
	}
	columns := func() int {
		t.Helper()
		var n int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('team_queue_waits') WHERE name='resume'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.exec(t, `DROP TABLE team_queue_waits`)
	f.exec(t, old)
	if columns() != 0 {
		t.Fatal("the old table has resume")
	}
	f.exec(t, `INSERT INTO team_queue_waits(task_id,entry_id,item_id,on_entry_id,on_item_id,paths_json,until,state,set_by,set_at,reason) VALUES(?,?,?,?,?,'[]','accepted','waiting','owner',?,?)`,
		f.task.ID, f.waiting.ID, f.items[1].ID, f.pred.ID, f.items[0].ID, ts(f.clock), waitPause)
	f.reopen(t)
	if columns() != 1 || f.resume(t) != "" {
		t.Fatalf("migrated %d resume columns, resume %q", columns(), f.resume(t))
	}
	if listed := f.waitingEntry(t); len(listed.Waits) != 1 || listed.Waits[0].Resume != "" || listed.Waits[0].Reason != waitPause {
		t.Fatalf("listed %+v", listed.Waits)
	}
	f.meet(t, api.TeamQueueWaitAccepted)
	if steps := f.sweep(t); len(steps) != 1 || steps[0] != "wait-met" {
		t.Fatalf("steps %v", steps)
	}
	got := f.notices(t, f.lead, queueWaitMetSubject)
	if len(got) != 1 || !strings.HasSuffix(got[0].Envelope.Body.Text, "Paused for: "+waitPause+". Resume the paused work.") || len(f.notices(t, f.lead, queueWaitGatedSubject)) != 0 {
		t.Fatalf("notices %+v", got)
	}
	f.reopen(t)
	if columns() != 1 || f.state(t) != api.TeamQueueWaitMet {
		t.Fatalf("second open: %d resume columns, state %s", columns(), f.state(t))
	}
}
