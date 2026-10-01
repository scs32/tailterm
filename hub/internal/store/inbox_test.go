package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// wi_a31c079e98518efc (#11759): a message addressed to a work-item-bound agent
// reaches its inbox and unread count whether or not it links that item, while
// board-wide and broadcast traffic for other items stays out.
func TestBoundInboxIncludesUnlinkedDirected(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	other, err := f.s.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Another item", AgentID: f.lead.ID, RequestID: "inbox-other-item"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	posted := 0
	post := func(req api.PostMessageRequest) api.Message {
		t.Helper()
		posted++
		if len(req.WorkItems) > 0 {
			req.RequestID = fmt.Sprintf("inbox-linked-%d", posted)
		}
		m, err := f.s.PostMessage(ctx, f.task.ID, req, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	otherLink := []api.MessageWorkItem{{ItemTaskID: other.TaskID, ItemID: other.ID, ItemRevision: other.Revision, Relationship: "primary"}}
	ownLink := []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}
	orderRef := &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}

	human := post(api.PostMessageRequest{Text: "unlinked owner instruction", To: f.worker.ID})
	agentText := post(api.PostMessageRequest{Text: "unlinked lead free text", To: f.worker.ID, AgentID: f.lead.ID})
	elsewhere := post(api.PostMessageRequest{Text: "directed but linked to another item", To: f.worker.ID, WorkItems: otherLink})
	board := post(api.PostMessageRequest{Text: "unlinked board-wide post"})
	own := post(api.PostMessageRequest{Text: "board-wide post for the bound item", WorkItems: ownLink, WorkOrderMessage: orderRef})
	self := post(api.PostMessageRequest{Text: "worker's own progress", AgentID: f.worker.ID, WorkItems: ownLink, WorkOrderMessage: orderRef})
	toLead := post(api.PostMessageRequest{Text: "unlinked message to the lead", To: f.lead.ID})
	if _, err = f.s.db.ExecContext(ctx, `UPDATE tasks SET swarm=1 WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	broadcastElsewhere := post(api.PostMessageRequest{Text: "broadcast linked to another item", To: f.lead.ID, WorkItems: otherLink})
	if !broadcastElsewhere.Broadcast {
		t.Fatalf("fixture did not broadcast: %+v", broadcastElsewhere)
	}
	broadcastDirected := post(api.PostMessageRequest{Text: "broadcast addressed to the worker", To: f.worker.ID, WorkItems: otherLink})

	inbox, err := f.s.ListMessages(ctx, f.task.ID, f.worker.ReadUpTo, f.worker.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, m := range inbox {
		got[m.Seq] = true
	}
	for name, m := range map[string]api.Message{"human": human, "agent free text": agentText, "linked elsewhere": elsewhere,
		"own item": own, "self": self, "broadcast directed": broadcastDirected} {
		if !got[m.Seq] {
			t.Errorf("bound inbox is missing the %s message #%d: %+v", name, m.Seq, inbox)
		}
	}
	for name, m := range map[string]api.Message{"unlinked board": board, "to lead": toLead, "broadcast elsewhere": broadcastElsewhere} {
		if got[m.Seq] {
			t.Errorf("bound inbox leaked the %s message #%d", name, m.Seq)
		}
	}
	if len(inbox) != 6 {
		t.Errorf("bound inbox has %d messages, want 6: %+v", len(inbox), inbox)
	}
	nonSelf := 0
	for _, m := range inbox {
		if m.From.AgentID != f.worker.ID {
			nonSelf++
		}
	}
	unread, err := f.s.Unread(ctx, f.task.ID, f.worker.ID)
	if err != nil || unread != nonSelf || unread != 5 {
		t.Errorf("Unread = %d (%v), want the %d non-self inbox messages", unread, err, nonSelf)
	}

	// Directed-only newest paging returns only what is addressed to the worker.
	directed, err := f.s.ListMessagesPage(ctx, f.task.ID, api.MessagePageQuery{After: f.worker.ReadUpTo, To: f.worker.ID, DirectedOnly: true, Newest: true, Limit: 2})
	if err != nil || len(directed) != 2 || directed[0].Seq != elsewhere.Seq || directed[1].Seq != broadcastDirected.Seq {
		t.Errorf("directed newest page: %+v %v", directed, err)
	}
	if _, err = f.s.ListMessagesPage(ctx, f.task.ID, api.MessagePageQuery{DirectedOnly: true}); err == nil {
		t.Error("directed paging without a recipient was accepted")
	}
}

// A wake prompt shows only a short subject per obligation, so it names the
// command that prints a message in full (wi_a31c079e98518efc, i3).
func TestObligationWakePromptNamesFullTextCommand(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	text := strings.Repeat("owner instruction ", 10) + "FULL-TAIL"
	m, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: text, To: f.worker.ID}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	obligations, err := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: f.worker.ID, OpenOnly: true}, f.s.now())
	if err != nil || len(obligations) != 1 || obligations[0].MessageSeq != m.Seq || strings.Contains(obligations[0].Subject, "FULL-TAIL") {
		t.Fatalf("expected one obligation with a truncated subject: %+v %v", obligations, err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: f.worker.ID, RunID: f.worker.RunID}, f.by); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.LeaseWakeJob(ctx, f.task.ID, f.worker.ID, f.worker.RunID, f.s.now())
	if err != nil || job == nil {
		t.Fatalf("no wake job: %+v %v", job, err)
	}
	if !strings.Contains(job.Prompt, "#"+itoa(m.Seq)+" human") || !strings.Contains(job.Prompt, "`tt inbox --seq SEQ`") {
		t.Fatalf("wake prompt does not name the full-text command: %q", job.Prompt)
	}
	full, err := f.s.ListMessagesPage(ctx, f.task.ID, api.MessagePageQuery{After: m.Seq - 1, Limit: 1})
	if err != nil || len(full) != 1 || full[0].Seq != m.Seq || full[0].Text != text {
		t.Fatalf("single-message page did not return the full text: %+v %v", full, err)
	}
}

// inboxScale is the wi_e1b6ca74b7e58414 fixture: 500 agents, 450 of them
// closed, 480 bound across 60 work items, and 20,000 messages.
type inboxScale struct {
	agents []string
	// boundOpen and unboundOpen are open agents that have read nothing, the
	// worst case for a count; closedBound is closed and has read nothing.
	boundOpen, unboundOpen, closedBound string
	// unlinkedDirected is addressed to boundOpen, links no item, and is the
	// second oldest message in the task.
	unlinkedDirected int64
	// unread is the expected count per agent that has read nothing.
	unread map[string]int
	// recent is the cursor of every other open agent.
	recent int64
}

const (
	inboxScaleAgents   = 500
	inboxScaleBound    = 480
	inboxScaleMessages = 20000
)

// seedInboxScale writes the fixture with plain SQL in one transaction, so
// setup stays well under a second. items are the 60 work items to bind to.
func seedInboxScale(t *testing.T, db *sql.DB, taskID string, items []api.WorkItem) inboxScale {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(stmt *sql.Stmt, args ...any) sql.Result {
		t.Helper()
		res, err := stmt.Exec(args...)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	prepare := func(q string) *sql.Stmt {
		t.Helper()
		stmt, err := tx.Prepare(q)
		if err != nil {
			t.Fatal(err)
		}
		return stmt
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	f := inboxScale{unread: map[string]int{}}
	open := func(i int) bool { return i%10 == 0 }
	itemOf := func(i int) int {
		if i >= inboxScaleBound {
			return -1
		}
		return i % len(items)
	}
	addAgent := prepare(`INSERT INTO agents (id,task_id,name,host,session,runtime,status,created_at,last_event_at,run_id) VALUES (?,?,?,?,?,?,?,?,?,?)`)
	for i := 0; i < inboxScaleAgents; i++ {
		id := fmt.Sprintf("agt_%016x", i+1)
		status := api.AgentClosed
		if open(i) {
			status = api.AgentRunning
		}
		exec(addAgent, id, taskID, fmt.Sprintf("scale-%d", i), "fixture", fmt.Sprintf("scale-%d", i), "codex", status, now, now, fmt.Sprintf("run_%016x", i+1))
		f.agents = append(f.agents, id)
	}
	f.boundOpen, f.unboundOpen, f.closedBound = f.agents[0], f.agents[inboxScaleBound], f.agents[1]

	// One third board-wide, the rest directed; every second message links an item.
	addMessage := prepare(`INSERT INTO messages (task_id,from_agent,from_node,from_user,to_agent,text,created_at) VALUES (?,?,?,?,?,?,?)`)
	addLink := prepare(`INSERT INTO message_work_item_links (message_seq,message_task_id,item_task_id,item_id,item_revision,relationship,created_at) VALUES (?,?,?,?,?,'primary',?)`)
	var first, last int64
	for j := 0; j < inboxScaleMessages; j++ {
		to, linked := -1, j%2 == 0
		if j%3 != 0 {
			to = (j * 7) % inboxScaleAgents
		}
		if j == 1 {
			to, linked = 0, false
		}
		from := (j*13 + 1) % inboxScaleAgents
		if from == to {
			from = (from + 1) % inboxScaleAgents
		}
		toID := ""
		if to >= 0 {
			toID = f.agents[to]
		}
		seq, err := exec(addMessage, taskID, f.agents[from], "fixture", "owner", toID, fmt.Sprintf("scale message %d", j), now).LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if j == 0 {
			first = seq
		}
		if j == 1 {
			f.unlinkedDirected = seq
		}
		last = seq
		item := -1
		if linked {
			item = j % len(items)
			exec(addLink, seq, taskID, items[item].TaskID, items[item].ID, items[item].Revision, now)
		}
		for _, reader := range []int{0, 1, inboxScaleBound} {
			if from == reader {
				continue
			}
			directed := to == reader
			boardWide := to < 0 && (itemOf(reader) < 0 || item == itemOf(reader))
			if directed || boardWide {
				f.unread[f.agents[reader]]++
			}
		}
	}
	f.recent = last - 100

	addBinding := prepare(`INSERT INTO agent_work_item_bindings (agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,team_role,context_digest,context_json,created_at) VALUES (?,?,?,?,?,?,?,0,'member',?,'{}',?)`)
	addCursor := prepare(`INSERT INTO read_cursors (task_id,agent_id,up_to) VALUES (?,?,?)`)
	for i, id := range f.agents {
		if item := itemOf(i); item >= 0 {
			exec(addBinding, id, fmt.Sprintf("run_%016x", i+1), items[item].TaskID, items[item].ID, items[item].Revision, taskID, first, strings.Repeat("0", 64), now)
		}
		if open(i) && id != f.boundOpen && id != f.unboundOpen {
			exec(addCursor, taskID, id, f.recent)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

// raceBuilt reports whether this test binary was built with -race, where
// wall-clock bounds do not hold.
func raceBuilt() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, setting := range info.Settings {
		if setting.Key == "-race" {
			return setting.Value == "true"
		}
	}
	return false
}

// bestOf returns the shortest of three runs of fn.
func bestOf(t *testing.T, fn func()) time.Duration {
	t.Helper()
	best := time.Duration(0)
	for run := 0; run < 3; run++ {
		start := time.Now()
		fn()
		if took := time.Since(start); run == 0 || took < best {
			best = took
		}
	}
	return best
}

// wi_e1b6ca74b7e58414: the inbox of a bound agent is read through the two
// indexes rather than a scan of the task's messages, and the roster counts
// unread only for agents that can still read, so both stay fast at 500 agents
// and 20,000 messages (213fd7e took 25 s for the roster at 473 agents).
func TestInboxScale(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "scale.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Inbox scale"}, by)
	if err != nil {
		t.Fatal(err)
	}
	items := []api.WorkItem{}
	for i := 0; i < 60; i++ {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: fmt.Sprintf("Scale item %d", i), RequestID: fmt.Sprintf("scale-item-%d", i)}, by)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	f := seedInboxScale(t, s.db, task.ID, items)

	// The index is declared in the schema, so Open creates it on a new
	// database and on an existing one that lacks it.
	hasIndex := func(when string) {
		t.Helper()
		var indexSQL string
		if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name='messages_task_to'`).Scan(&indexSQL); err != nil || !strings.Contains(indexSQL, "(task_id, to_agent, seq)") {
			t.Fatalf("messages_task_to index on %s database: %q %v", when, indexSQL, err)
		}
	}
	hasIndex("a new")
	if _, err = s.db.ExecContext(ctx, `DROP INDEX messages_task_to`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	hasIndex("an existing")

	// Correctness on the fixture.
	for _, id := range []string{f.boundOpen, f.unboundOpen} {
		unread, err := s.Unread(ctx, task.ID, id)
		if err != nil || unread != f.unread[id] || unread == 0 {
			t.Errorf("Unread(%s) = %d (%v), want %d", id, unread, err, f.unread[id])
		}
	}
	nonSelf, found := 0, false
	for after := int64(0); ; {
		page, err := s.ListMessagesPage(ctx, task.ID, api.MessagePageQuery{After: after, To: f.boundOpen})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page {
			if m.From.AgentID != f.boundOpen {
				nonSelf++
			}
			found = found || m.Seq == f.unlinkedDirected
			after = m.Seq
		}
		if len(page) < api.MaxLimit {
			break
		}
	}
	if !found || nonSelf != f.unread[f.boundOpen] {
		t.Errorf("bound page: unlinked directed #%d found=%v, %d non-self messages, want %d", f.unlinkedDirected, found, nonSelf, f.unread[f.boundOpen])
	}

	// Query shape: both indexes, no per-row subquery and no scan of messages.
	boundAgent, err := s.GetAgent(ctx, f.boundOpen)
	if err != nil || boundAgent.WorkItem == nil {
		t.Fatalf("bound agent: %+v %v", boundAgent, err)
	}
	plan := func(q string, args []any) string {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(details, "\n")
	}
	unreadQuery, unreadArgs := inboxUnreadQuery(task.ID, f.boundOpen, 0, boundAgent.WorkItem)
	pageQuery, pageArgs := inboxPageQuery(task.ID, api.MessagePageQuery{To: f.boundOpen}, boundAgent.WorkItem, api.MaxLimit)
	newestQuery, newestArgs := inboxPageQuery(task.ID, api.MessagePageQuery{To: f.boundOpen, Before: f.recent, Newest: true}, boundAgent.WorkItem, 50)
	for name, got := range map[string]string{"unread": plan(unreadQuery, unreadArgs), "page": plan(pageQuery, pageArgs), "newest page": plan(newestQuery, newestArgs)} {
		for _, index := range []string{"messages_task_to", "message_work_item_links_item"} {
			if !strings.Contains(got, index) {
				t.Errorf("%s plan does not use %s:\n%s", name, index, got)
			}
		}
		for _, banned := range []string{"CORRELATED", "SCAN m"} {
			if strings.Contains(got, banned) {
				t.Errorf("%s plan contains %s:\n%s", name, banned, got)
			}
		}
	}

	// The roster counts unread only for agents that can still read.
	var roster []api.Agent
	listAgents := func() {
		if roster, err = s.ListAgents(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	rosterTime := bestOf(t, listAgents)
	if len(roster) != inboxScaleAgents {
		t.Fatalf("roster has %d agents, want %d", len(roster), inboxScaleAgents)
	}
	closed := 0
	for _, a := range roster {
		switch {
		case a.Status == api.AgentClosed:
			closed++
			if a.Unread != 0 {
				t.Fatalf("roster counted %d unread for closed agent %s", a.Unread, a.ID)
			}
		case a.ID == f.boundOpen || a.ID == f.unboundOpen:
			if a.Unread != f.unread[a.ID] {
				t.Errorf("roster unread for %s = %d, want %d", a.ID, a.Unread, f.unread[a.ID])
			}
		}
	}
	if closed != 450 {
		t.Errorf("roster has %d closed agents, want 450", closed)
	}
	closedAgent, err := s.GetAgent(ctx, f.closedBound)
	if err != nil || closedAgent.Unread != f.unread[f.closedBound] || closedAgent.Unread == 0 {
		t.Errorf("GetAgent(closed).Unread = %d (%v), want %d", closedAgent.Unread, err, f.unread[f.closedBound])
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE agents SET status=? WHERE id=?`, api.AgentExited, f.boundOpen); err != nil {
		t.Fatal(err)
	}
	listAgents()
	if roster[0].ID != f.boundOpen || roster[0].Status != api.AgentExited || roster[0].Unread != 0 {
		t.Errorf("roster counted unread for an exited agent: %+v", roster[0])
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE agents SET status=? WHERE id=?`, api.AgentRunning, f.boundOpen); err != nil {
		t.Fatal(err)
	}

	inboxTime := bestOf(t, func() {
		cursor, err := s.ReadCursor(ctx, task.ID, f.boundOpen)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Unread(ctx, task.ID, f.boundOpen); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ListMessagesPage(ctx, task.ID, api.MessagePageQuery{After: cursor, To: f.boundOpen}); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("500 agents, 20,000 messages: ListAgents %v, unread inbox %v", rosterTime, inboxTime)
	if testing.Short() || raceBuilt() {
		t.Log("wall-clock bounds are not checked in short or race runs")
		return
	}
	if rosterTime > time.Second {
		t.Errorf("ListAgents took %v, want under 1 s", rosterTime)
	}
	if inboxTime > time.Second {
		t.Errorf("unread inbox took %v, want under 1 s", inboxTime)
	}
}
