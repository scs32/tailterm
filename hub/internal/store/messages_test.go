package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// wi_85bfbdaa5e09fb64 (#16632): an item's lead is told once about a typed
// RESULT, BLOCK or DECLINE reply on its item that is addressed to a teammate,
// and its inbox can show that reply.
type leadCopyFixture struct {
	s        *Store
	ctx      context.Context
	by       api.Caller
	task     api.Task
	items    []api.WorkItem
	orders   []api.Message
	handler  api.Agent // unbound database handler
	lead     api.Agent // bound to items[0] and its running lead
	verifier api.Agent // bound to items[0]
	member   api.Agent // bound to items[0]
	other    api.Agent // bound to items[1] and its running lead
	posted   int
}

func newLeadCopyFixture(t *testing.T) *leadCopyFixture {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	f := &leadCopyFixture{s: s, ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}, task: task, items: items, orders: orders}
	handler, err := scanAgent(s.db.QueryRowContext(f.ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name='database'`, task.ID))
	if err != nil {
		t.Fatal(err)
	}
	f.handler = handler
	add := func(name string, item int, lead bool) api.Agent {
		t.Helper()
		a, err := s.AddAgent(f.ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "mini", Session: name, Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,0,?,?,?)`,
			a.ID, a.RunID, task.ID, items[item].ID, items[item].Revision, task.ID, orders[item].Seq, "digest", []byte("{}"), ts(s.now())); err != nil {
			t.Fatal(err)
		}
		if lead {
			if _, err = s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, task.ID, items[item].ID, a.ID, a.RunID); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	f.lead, f.verifier, f.member, f.other = add("lead-a", 0, true), add("verifier-a", 0, false), add("member-a", 0, false), add("lead-b", 1, true)
	return f
}

// send posts a typed message, linked to items[0] under its order when linked.
func (f *leadCopyFixture) send(t *testing.T, from, to api.Agent, replyTo int64, linked bool, env api.Envelope) api.Message {
	t.Helper()
	m, err := f.s.PostMessage(f.ctx, f.task.ID, f.request(from, to, replyTo, linked, env), f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *leadCopyFixture) request(from, to api.Agent, replyTo int64, linked bool, env api.Envelope) api.PostMessageRequest {
	f.posted++
	env.To = to.Name
	req := api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, To: to.ID, ReplyTo: replyTo, Envelope: &env}
	if linked {
		req.RequestID = fmt.Sprintf("lead-copy-test-%d", f.posted)
		req.WorkItems = []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.items[0].ID, ItemRevision: f.items[0].Revision, Relationship: "primary"}}
		req.WorkOrderMessage = &api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}
	}
	return req
}

func leadCopyEnvelope(kind string) api.Envelope {
	switch kind {
	case api.EnvelopeKindRequest:
		return api.Envelope{Kind: kind, Subject: "Confirm the verification import", Body: api.EnvelopeBody{Ask: "Confirm the saved import."}}
	case api.EnvelopeKindResult:
		return api.Envelope{Kind: kind, Subject: "The verification import is saved", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
			Evidence: map[string]api.Evidence{"e1": {Type: "record", Value: "read back"}}}
	case api.EnvelopeKindBlock:
		return api.Envelope{Kind: kind, Subject: "The verification import cannot be saved", Body: api.EnvelopeBody{Reason: "The plan is missing.", Needs: "The saved plan.", ResumeWhen: "The plan is saved."}}
	case api.EnvelopeKindDecline:
		return api.Envelope{Kind: kind, Subject: "The verification import is refused", Body: api.EnvelopeBody{Reason: "The import names another item."}}
	case api.EnvelopeKindAnswer:
		return api.Envelope{Kind: kind, Subject: "The verification import question", Body: api.EnvelopeBody{Answer: "Yes."}}
	}
	return api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "The verification import is under way", Body: api.EnvelopeBody{Text: "Importing now."}}
}

// exchange posts the verifier's REQUEST to the handler and the handler's
// reply of the given kind to the verifier.
func (f *leadCopyFixture) exchange(t *testing.T, kind string, linked bool) (request, reply api.Message) {
	t.Helper()
	request = f.send(t, f.verifier, f.handler, 0, linked, leadCopyEnvelope(api.EnvelopeKindRequest))
	reply = f.send(t, f.handler, f.verifier, request.Seq, linked, leadCopyEnvelope(kind))
	return request, reply
}

// copies returns the hub's lead copies addressed to agent.
func (f *leadCopyFixture) copies(t *testing.T, agent api.Agent) []api.Message {
	t.Helper()
	all, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", api.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range all {
		if m.To == agent.ID && m.From.Node == api.BrokerNode && m.Envelope != nil && m.Envelope.Refs["copy"] == "lead" {
			out = append(out, m)
		}
	}
	return out
}

// allCopies counts lead copies addressed to anyone.
func (f *leadCopyFixture) allCopies(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT count(*) FROM messages WHERE task_id=? AND from_node=?`, f.task.ID, api.BrokerNode).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// owed returns agent's obligation and wake-job counts.
func (f *leadCopyFixture) owed(t *testing.T, agent api.Agent) (obligations, wakes int) {
	t.Helper()
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM obligations WHERE task_id=? AND agent_id=?),(SELECT count(*) FROM wake_jobs WHERE task_id=? AND agent_id=?)`,
		f.task.ID, agent.ID, f.task.ID, agent.ID).Scan(&obligations, &wakes); err != nil {
		t.Fatal(err)
	}
	return obligations, wakes
}

func (f *leadCopyFixture) online(t *testing.T, agents ...api.Agent) {
	t.Helper()
	for _, a := range agents {
		if _, err := f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: a.ID, RunID: a.RunID}, f.by); err != nil {
			t.Fatal(err)
		}
	}
}

// g1, g3: a verifier REQUEST to the handler and the handler's linked RESULT to
// the verifier give the item's lead one notice, one delivery obligation and
// one wake job, and give the verifier nothing more.
func TestLinkedResultToTeammateCopiesItemLead(t *testing.T) {
	f := newLeadCopyFixture(t)
	request, result := f.exchange(t, api.EnvelopeKindResult, true)
	copies := f.copies(t, f.lead)
	if len(copies) != 1 || f.allCopies(t) != 1 {
		t.Fatalf("lead copies = %+v (%d hub messages), want exactly one", copies, f.allCopies(t))
	}
	c := copies[0]
	if c.Envelope.Kind != api.EnvelopeKindNotice || c.Envelope.To != f.lead.Name || c.ReplyTo != 0 || c.Broadcast || c.From.AgentID != "" ||
		c.Envelope.Refs["message"] != fmt.Sprint(result.Seq) || c.Envelope.Refs["replyTo"] != fmt.Sprint(request.Seq) {
		t.Fatalf("lead copy shape: %+v", c)
	}
	if c.Envelope.Subject != "Reply to "+f.verifier.Name+" on your item" {
		t.Fatalf("lead copy subject = %q", c.Envelope.Subject)
	}
	for _, want := range []string{f.handler.Name, f.verifier.Name, "RESULT", result.Envelope.Subject, fmt.Sprintf("tt inbox --seq %d", result.Seq), "No reply is needed."} {
		if !strings.Contains(c.Envelope.Body.Text, want) {
			t.Errorf("lead copy text lacks %q: %q", want, c.Envelope.Body.Text)
		}
	}
	if len(c.WorkItems) != 1 || c.WorkItems[0].ItemID != f.items[0].ID || c.WorkItems[0].Relationship != "primary" ||
		c.WorkOrderMessage == nil || c.WorkOrderMessage.Seq != f.orders[0].Seq {
		t.Fatalf("lead copy links: %+v %+v", c.WorkItems, c.WorkOrderMessage)
	}
	obligations, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.lead.ID}, f.s.now())
	if err != nil || len(obligations) != 1 || obligations[0].MessageSeq != c.Seq || obligations[0].Needs != api.ObligationNeedsDelivery {
		t.Fatalf("lead obligations: %+v %v", obligations, err)
	}
	if o, w := f.owed(t, f.lead); o != 1 || w != 1 {
		t.Fatalf("lead has %d obligations and %d wake jobs, want 1 and 1", o, w)
	}
	if o, w := f.owed(t, f.verifier); o != 0 || w != 0 {
		t.Fatalf("verifier has %d obligations and %d wake jobs, want none", o, w)
	}
	if o, w := f.owed(t, f.other); o != 0 || w != 0 || len(f.copies(t, f.other)) != 0 {
		t.Fatalf("another item's lead has %d obligations and %d wake jobs, want none", o, w)
	}
	// The delivery-only copy never gates the lead's own posts.
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, notice(f.lead, "still coordinating"), f.by); err != nil {
		t.Fatalf("the lead copy gated the lead: %v", err)
	}
}

// g3: the same exchange without item links is unchanged.
func TestUnlinkedResultCopiesNobody(t *testing.T) {
	f := newLeadCopyFixture(t)
	request, result := f.exchange(t, api.EnvelopeKindResult, false)
	if result.To != f.verifier.ID || len(result.WorkItems) != 0 {
		t.Fatalf("fixture result: %+v", result)
	}
	if n := f.allCopies(t); n != 0 {
		t.Fatalf("an unlinked RESULT produced %d hub messages", n)
	}
	for _, a := range []api.Agent{f.lead, f.verifier, f.member, f.other} {
		if o, w := f.owed(t, a); o != 0 || w != 0 {
			t.Errorf("%s has %d obligations and %d wake jobs, want none", a.Name, o, w)
		}
	}
	// Only the request's own obligation and wake job exist.
	var obligations, wakes int
	if err := f.s.db.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM obligations WHERE task_id=?),(SELECT count(*) FROM wake_jobs WHERE task_id=?)`, f.task.ID, f.task.ID).Scan(&obligations, &wakes); err != nil {
		t.Fatal(err)
	}
	if o, w := f.owed(t, f.handler); o != 1 || w != 1 || obligations != 1 || wakes != 1 {
		t.Fatalf("obligations %d/%d and wake jobs %d/%d, want only the handler's for request #%d", o, obligations, w, wakes, request.Seq)
	}
}

// g1: RESULT, BLOCK and DECLINE replies are copied; other kinds and a typed
// result that answers nothing are not.
func TestLeadCopyKinds(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want int
	}{
		{api.EnvelopeKindResult, 1}, {api.EnvelopeKindBlock, 1}, {api.EnvelopeKindDecline, 1},
		{api.EnvelopeKindAnswer, 0}, {api.EnvelopeKindNotice, 0},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newLeadCopyFixture(t)
			_, reply := f.exchange(t, tc.kind, true)
			copies := f.copies(t, f.lead)
			if len(copies) != tc.want || f.allCopies(t) != tc.want {
				t.Fatalf("%s reply produced %d lead copies (%d hub messages), want %d", tc.kind, len(copies), f.allCopies(t), tc.want)
			}
			if tc.want == 1 && (copies[0].Envelope.Refs["message"] != fmt.Sprint(reply.Seq) || !strings.Contains(copies[0].Envelope.Body.Text, strings.ToUpper(tc.kind))) {
				t.Fatalf("%s copy: %+v", tc.kind, copies[0])
			}
			if o, w := f.owed(t, f.lead); o != tc.want || w != tc.want {
				t.Fatalf("lead has %d obligations and %d wake jobs, want %d", o, w, tc.want)
			}
		})
	}
	t.Run("result that is not a reply", func(t *testing.T) {
		f := newLeadCopyFixture(t)
		f.send(t, f.handler, f.verifier, 0, true, leadCopyEnvelope(api.EnvelopeKindResult))
		if n := f.allCopies(t); n != 0 {
			t.Fatalf("a result without reply-to produced %d hub messages", n)
		}
	})
}

// g1, g2: no copy when the lead already has the reply or sent it, on a retry,
// on a swarm task, or when the item has no running live lead.
func TestLeadCopySkipsLeadAndRetries(t *testing.T) {
	t.Run("lead is the recipient", func(t *testing.T) {
		f := newLeadCopyFixture(t)
		request := f.send(t, f.lead, f.handler, 0, true, leadCopyEnvelope(api.EnvelopeKindRequest))
		f.send(t, f.handler, f.lead, request.Seq, true, leadCopyEnvelope(api.EnvelopeKindResult))
		if o, w := f.owed(t, f.lead); f.allCopies(t) != 0 || o != 0 || w != 0 {
			t.Fatalf("a reply to the lead produced %d hub messages, %d obligations, %d wake jobs", f.allCopies(t), o, w)
		}
	})
	t.Run("lead is the sender", func(t *testing.T) {
		f := newLeadCopyFixture(t)
		request := f.send(t, f.verifier, f.lead, 0, true, leadCopyEnvelope(api.EnvelopeKindRequest))
		f.send(t, f.lead, f.verifier, request.Seq, true, leadCopyEnvelope(api.EnvelopeKindResult))
		if n := f.allCopies(t); n != 0 {
			t.Fatalf("the lead's own reply produced %d hub messages", n)
		}
	})
	t.Run("retry", func(t *testing.T) {
		f := newLeadCopyFixture(t)
		request := f.send(t, f.verifier, f.handler, 0, true, leadCopyEnvelope(api.EnvelopeKindRequest))
		reply := f.request(f.handler, f.verifier, request.Seq, true, leadCopyEnvelope(api.EnvelopeKindResult))
		first, err := f.s.PostMessage(f.ctx, f.task.ID, reply, f.by)
		if err != nil {
			t.Fatal(err)
		}
		again, err := f.s.PostMessage(f.ctx, f.task.ID, reply, f.by)
		if err != nil || again.Seq != first.Seq {
			t.Fatalf("retry: %+v %v", again, err)
		}
		if o, w := f.owed(t, f.lead); len(f.copies(t, f.lead)) != 1 || f.allCopies(t) != 1 || o != 1 || w != 1 {
			t.Fatalf("a retry left %d hub messages, %d obligations, %d wake jobs, want one each", f.allCopies(t), o, w)
		}
	})
	t.Run("swarm task", func(t *testing.T) {
		f := newLeadCopyFixture(t)
		if _, err := f.s.db.ExecContext(f.ctx, `UPDATE tasks SET swarm=1 WHERE id=?`, f.task.ID); err != nil {
			t.Fatal(err)
		}
		_, reply := f.exchange(t, api.EnvelopeKindResult, true)
		if !reply.Broadcast || f.allCopies(t) != 0 {
			t.Fatalf("a broadcast reply (%v) produced %d hub messages", reply.Broadcast, f.allCopies(t))
		}
	})
	for name, change := range map[string]string{
		"closed lead row":    `UPDATE item_team_leads SET state='closed' WHERE agent_id=?`,
		"launching lead row": `UPDATE item_team_leads SET state='launching' WHERE agent_id=?`,
		"replaced lead run":  `UPDATE item_team_leads SET run_id='run_0000000000000000' WHERE agent_id=?`,
		"retired lead":       `UPDATE agents SET status='retired' WHERE id=?`,
		"closed lead":        `UPDATE agents SET status='closed' WHERE id=?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newLeadCopyFixture(t)
			if _, err := f.s.db.ExecContext(f.ctx, change, f.lead.ID); err != nil {
				t.Fatal(err)
			}
			f.exchange(t, api.EnvelopeKindResult, true)
			if o, w := f.owed(t, f.lead); f.allCopies(t) != 0 || o != 0 || w != 0 {
				t.Fatalf("%s: %d hub messages, %d obligations, %d wake jobs, want none", name, f.allCopies(t), o, w)
			}
		})
	}
}

// g2, g3: the idle lead is woken once, for the copy, and the requester gets no
// wake job at all.
func TestLeadCopyWakesOnce(t *testing.T) {
	f := newLeadCopyFixture(t)
	f.online(t, f.lead, f.verifier)
	if _, err := f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{Kind: api.EventDone, AgentID: f.lead.ID, RunID: f.lead.RunID}, f.by); err != nil {
		t.Fatal(err)
	}
	f.exchange(t, api.EnvelopeKindResult, true)
	copies := f.copies(t, f.lead)
	if len(copies) != 1 {
		t.Fatalf("lead copies: %+v", copies)
	}
	now := f.s.now()
	job, err := f.s.LeaseWakeJob(f.ctx, f.task.ID, f.lead.ID, f.lead.RunID, now)
	if err != nil || job == nil || job.MessageSeq != copies[0].Seq || !strings.Contains(job.Prompt, fmt.Sprintf("#%d notice", copies[0].Seq)) {
		t.Fatalf("lead wake job: %+v %v", job, err)
	}
	if err = f.s.ReportWakeJob(f.ctx, f.task.ID, job.ID, api.WakeJobReport{LeaseToken: job.LeaseToken, Status: wakeAccepted}, now); err != nil {
		t.Fatal(err)
	}
	if again, err := f.s.LeaseWakeJob(f.ctx, f.task.ID, f.lead.ID, f.lead.RunID, now.Add(wakeLease+1)); err != nil || again != nil {
		t.Fatalf("the lead was woken twice: %+v %v", again, err)
	}
	if requester, err := f.s.LeaseWakeJob(f.ctx, f.task.ID, f.verifier.ID, f.verifier.RunID, now); err != nil || requester != nil {
		t.Fatalf("the requester got a wake job: %+v %v", requester, err)
	}
	if _, w := f.owed(t, f.lead); w != 1 {
		t.Fatalf("lead has %d wake jobs, want 1", w)
	}
}

// g5: a lead's inbox shows messages linked to its item whoever they are
// addressed to; a member's and another item's lead's do not, and the unread
// count is unchanged by the teammates' traffic.
func TestLeadReadsLinkedMessagesToTeammates(t *testing.T) {
	f := newLeadCopyFixture(t)
	unread := func(a api.Agent) int {
		t.Helper()
		n, err := f.s.Unread(f.ctx, f.task.ID, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := map[string]int{f.lead.Name: unread(f.lead), f.member.Name: unread(f.member), f.other.Name: unread(f.other)}
	request, result := f.exchange(t, api.EnvelopeKindResult, true)
	copies := f.copies(t, f.lead)
	if len(copies) != 1 {
		t.Fatalf("lead copies: %+v", copies)
	}
	sees := func(a api.Agent, page api.MessagePageQuery) map[int64]bool {
		t.Helper()
		page.To = a.ID
		messages, err := f.s.ListMessagesPage(f.ctx, f.task.ID, page)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int64]bool{}
		for _, m := range messages {
			got[m.Seq] = true
		}
		return got
	}
	inbox, err := f.s.ListMessages(f.ctx, f.task.ID, 0, f.lead.ID, api.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int64]bool{}
	for _, m := range inbox {
		listed[m.Seq] = true
	}
	if !listed[request.Seq] || !listed[result.Seq] || !listed[copies[0].Seq] || !listed[f.orders[0].Seq] || listed[f.orders[1].Seq] {
		t.Fatalf("lead inbox: %v", listed)
	}
	// tt inbox --seq N reads the page strictly between N-1 and N+1.
	one := api.MessagePageQuery{After: result.Seq - 1, Before: result.Seq + 1}
	if got := sees(f.lead, one); len(got) != 1 || !got[result.Seq] {
		t.Fatalf("lead cannot read the RESULT addressed to its verifier: %v", got)
	}
	if got := sees(f.lead, api.MessagePageQuery{Before: result.Seq + 1, Newest: true, Limit: 1}); len(got) != 1 || !got[result.Seq] {
		t.Fatalf("lead newest page: %v", got)
	}
	if got := sees(f.verifier, one); !got[result.Seq] {
		t.Fatalf("the verifier lost its own RESULT: %v", got)
	}
	for _, a := range []api.Agent{f.member, f.other} {
		if got := sees(a, api.MessagePageQuery{}); got[request.Seq] || got[result.Seq] || got[copies[0].Seq] {
			t.Errorf("%s reads its teammates' messages: %v", a.Name, got)
		}
	}
	// Directed-only paging stays what is addressed to the lead.
	if got := sees(f.lead, api.MessagePageQuery{DirectedOnly: true}); len(got) != 1 || !got[copies[0].Seq] {
		t.Fatalf("lead directed page: %v", got)
	}
	// A lead whose mapping is closed reads like any member again.
	if _, err = f.s.db.ExecContext(f.ctx, `UPDATE item_team_leads SET state='closed' WHERE agent_id=?`, f.other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.ExecContext(f.ctx, `UPDATE agent_work_item_bindings SET item_id=? WHERE agent_id=?`, f.items[0].ID, f.other.ID); err != nil {
		t.Fatal(err)
	}
	if got := sees(f.other, one); len(got) != 0 {
		t.Fatalf("a non-lead bound to the item reads the RESULT: %v", got)
	}
	if got := unread(f.lead) - before[f.lead.Name]; got != 1 {
		t.Errorf("lead unread grew by %d, want 1 (the copy only)", got)
	}
	if got := unread(f.member) - before[f.member.Name]; got != 0 {
		t.Errorf("member unread grew by %d, want 0", got)
	}
}
