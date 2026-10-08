package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestUnaddressedReplyToLiveAuthorAcknowledgesThroughGate(t *testing.T) {
	f := newPhase3Fixture(t)
	start := time.Now().UTC().Truncate(time.Second)
	f.s.now = func() time.Time { return start }
	assign := f.assignTo(t, f.builder)
	f.s.now = func() time.Time { return start.Add(api.ObligationAckGrace + time.Second) }
	stale := api.PostMessageRequest{AgentID: f.builder.ID, RunID: "run_0000000000000000", ReplyTo: assign.Seq, Text: "Fixture done"}
	var unacked *api.UnacknowledgedError
	if _, err := f.post(t, stale); !errors.As(err, &unacked) {
		t.Fatalf("stale run bypassed acknowledgement gate: %v", err)
	}
	reply := stale
	reply.RunID = f.builder.RunID
	message, err := f.post(t, reply)
	if err != nil || message.To != f.lead.ID || message.ReplyTo != assign.Seq {
		t.Fatalf("unaddressed reply was not routed to live author: %+v %v", message, err)
	}
	if o := f.obligationFor(t, assign.Seq); o.State != api.ObligationAcknowledged || o.AckedAt == nil {
		t.Fatalf("routed reply did not acknowledge assignment: %+v", o)
	}
	if _, err := f.post(t, notice(f.builder, "acknowledged through reply")); err != nil {
		t.Fatalf("routed reply did not release gate: %v", err)
	}
}

func TestReplyRouting(t *testing.T) {
	f := newPhase3Fixture(t)
	post := func(req api.PostMessageRequest) api.Message {
		t.Helper()
		m, err := f.post(t, req)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// The lead's request and the handler's unaddressed result have the
	// #9678/#9680 shape. The stored recipient drives Board and inbox reads.
	parent := post(api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.handler.ID, Text: "Record the correction order"})
	reply := post(api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: parent.Seq, Text: "Correction order recorded"})
	if reply.To != f.lead.ID || reply.ReplyTo != parent.Seq || reply.Broadcast || reply.Envelope != nil {
		t.Fatalf("unaddressed reply lost its stored target: %+v", reply)
	}
	board, err := f.s.ListMessages(f.ctx, f.task.ID, parent.Seq, "", 10)
	if err != nil || len(board) != 1 || board[0].To != f.lead.ID {
		t.Fatalf("Board recipient: %+v %v", board, err)
	}
	inbox, err := f.s.ListMessages(f.ctx, f.task.ID, parent.Seq, f.lead.ID, 10)
	if err != nil || len(inbox) != 1 || inbox[0].Seq != reply.Seq {
		t.Fatalf("sender inbox: %+v %v", inbox, err)
	}
	assigned := post(api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.handler.ID,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: f.handler.Name, Subject: "Verify the correction evidence", Body: api.EnvelopeBody{Ask: "Check the saved result"}}})
	result := post(api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: assigned.Seq,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindResult, Subject: "Correction evidence is verified", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "record", Value: "verified"}}}})
	if result.To != f.lead.ID || f.obligationFor(t, assigned.Seq).State != api.ObligationClosed {
		t.Fatalf("unaddressed RESULT did not reach lead and close request: %+v %+v", result, f.obligationFor(t, assigned.Seq))
	}
	if _, err := f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Kind: api.EventDone}, f.by); err != nil {
		t.Fatal(err)
	}
	lead, err := f.s.GetAgent(f.ctx, f.lead.ID)
	if err != nil || lead.Unread == 0 {
		t.Fatalf("idle sender unread: %+v %v", lead, err)
	}

	// A typed question with refs.repliesTo creates exactly one obligation for
	// the inferred recipient, while preserving the author's envelope text.
	question := post(api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindQuestion, Subject: "Which correction should proceed", Refs: map[string]string{"repliesTo": "#" + strconv.FormatInt(parent.Seq, 10)}, Body: api.EnvelopeBody{Question: "Use this correction?"}}})
	if question.To != f.lead.ID || question.ReplyTo != parent.Seq || question.Envelope.To != "" || question.Text != api.RenderText(*question.Envelope) {
		t.Fatalf("typed reply: %+v", question)
	}
	if o := f.obligationFor(t, question.Seq); o.AgentID != f.lead.ID {
		t.Fatalf("typed reply obligation: %+v", o)
	}
	var count int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM obligations WHERE message_seq=?`, question.Seq).Scan(&count); err != nil || count != 1 {
		t.Fatalf("typed reply obligation count = %d, %v", count, err)
	}

	// An explicit alternate recipient, a role recipient, a person's post and
	// a self reply retain their existing routing.
	alt := post(api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, To: f.builder.ID, ReplyTo: parent.Seq, Text: "Builder owns this"})
	if alt.To != f.builder.ID {
		t.Fatalf("explicit alternate recipient: %+v", alt)
	}
	roleReply := api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, ReplyTo: parent.Seq,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, To: "role:database_handler", Subject: "Retain the handler copy", Body: api.EnvelopeBody{Text: "For the handler"}}}
	role := post(roleReply)
	if role.To != f.handler.ID || role.Envelope.To != "role:database_handler" {
		t.Fatalf("role recipient: %+v", role)
	}
	// The same reply from the role's own holder would reach only its sender.
	roleReply.AgentID, roleReply.RunID = f.handler.ID, f.handler.RunID
	if m, err := f.post(t, roleReply); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), selfAddressedMessage) {
		t.Fatalf("reply to a role its sender holds: %+v %v", m, err)
	}
	humanParent := post(api.PostMessageRequest{Text: "Owner request"})
	personReply := post(api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: humanParent.Seq, Text: "Owner answer"})
	if personReply.To != "" {
		t.Fatalf("reply to a person: %+v", personReply)
	}
	self := post(api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: reply.Seq, Text: "Own follow-up"})
	if self.To != "" {
		t.Fatalf("self reply: %+v", self)
	}
	if _, err := f.post(t, api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, To: f.builder.ID, ReplyTo: parent.Seq,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, To: f.lead.Name, Subject: "Mismatched destination", Body: api.EnvelopeBody{Text: "Wrong target"}}}); err == nil {
		t.Fatal("explicit mismatch was accepted")
	}
	if _, err := f.post(t, api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: 999999, Text: "Missing parent"}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("missing parent: %v", err)
	}
	other, err := f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Another task"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PostMessage(f.ctx, other.ID, api.PostMessageRequest{ReplyTo: parent.Seq, Text: "Cross-task"}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("cross-task parent: %v", err)
	}
	retired := api.AgentRetired
	if _, err := f.s.UpdateAgent(f.ctx, f.lead.ID, api.UpdateAgentRequest{Status: &retired}, f.by); err != nil {
		t.Fatal(err)
	}
	retiredReply := post(api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: parent.Seq, Text: "Stored while lead is retired"})
	retiredLead, err := f.s.GetAgent(f.ctx, f.lead.ID)
	if err != nil || retiredReply.To != f.lead.ID || retiredLead.Status != api.AgentRetired {
		t.Fatalf("reply resumed a retired agent: %+v %+v %v", retiredReply, retiredLead, err)
	}
}

func TestReplyRoutingRetryAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	by := api.Caller{Node: "workspace", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Reply replay"}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string) api.Agent {
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "h", Session: name, Runtime: "codex"}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead, handler := add("lead"), add("handler")
	parent, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, RunID: lead.RunID, To: handler.ID, Text: "Need correction"}, by)
	if err != nil {
		t.Fatal(err)
	}
	req := api.PostMessageRequest{AgentID: handler.ID, RunID: handler.RunID, ReplyTo: parent.Seq, RequestID: "reply-retry",
		Envelope: &api.Envelope{Kind: api.EnvelopeKindQuestion, Subject: "Confirm the correction outcome", Body: api.EnvelopeBody{Question: "Is the correction accepted?"}}}
	first, err := s.PostMessage(ctx, task.ID, req, by)
	if err != nil || first.To != lead.ID {
		t.Fatalf("initial post: %+v %v", first, err)
	}
	counts := func() [3]int {
		var out [3]int
		for i, table := range []string{"events", "obligations", "wake_jobs"} {
			if err := s.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE task_id=?`, task.ID).Scan(&out[i]); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before := counts()
	if before[1] != 1 || before[2] != 1 {
		t.Fatalf("typed question did not create one obligation and wake job: %v", before)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replayed, err := s.PostMessage(ctx, task.ID, req, by)
	if err != nil || replayed.Seq != first.Seq || replayed.To != lead.ID || replayed.PostReceipt == nil {
		t.Fatalf("receipt replay: %+v %v", replayed, err)
	}
	msgs, err := s.ListMessages(ctx, task.ID, parent.Seq, "", 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("replay inserted a second message: %+v %v", msgs, err)
	}
	if after := counts(); after != before {
		t.Fatalf("receipt replay changed events/obligations/wake jobs: before=%v after=%v", before, after)
	}
}

func TestReplyRoutingSwarmStillBroadcasts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "workspace", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Swarm replies", Swarm: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string) api.Agent {
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "h", Session: name, Runtime: "codex"}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead, handler, peer := add("lead"), add("handler"), add("peer")
	parent, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, RunID: lead.RunID, To: handler.ID, Text: "Send the result"}, by)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: handler.ID, RunID: handler.RunID, ReplyTo: parent.Seq, Text: "Result ready"}, by)
	if err != nil || reply.To != lead.ID || !reply.Broadcast {
		t.Fatalf("swarm reply lost recipient or broadcast: %+v %v", reply, err)
	}
	peerInbox, err := s.ListMessages(ctx, task.ID, parent.Seq, peer.ID, 10)
	if err != nil || len(peerInbox) != 1 || peerInbox[0].Seq != reply.Seq {
		t.Fatalf("peer did not receive broadcast: %+v %v", peerInbox, err)
	}
}

func TestReplyRoutingClosedAndExitedAuthorsStayBoardWide(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"closed", api.AgentClosed},
		{"exited", api.AgentExited},
		{"done", api.AgentDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPhase3Fixture(t)
			parent, err := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Text: "Old lead request"})
			if err != nil {
				t.Fatal(err)
			}
			switch tc.status {
			case api.AgentClosed:
				_, err = f.s.CloseAgent(f.ctx, f.lead.ID, f.by)
			case api.AgentExited:
				_, err = f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Kind: api.EventExited}, f.by)
			case api.AgentDone:
				_, err = f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Kind: api.EventDone}, f.by)
			}
			if err != nil {
				t.Fatal(err)
			}
			reply, err := f.post(t, api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: parent.Seq, Text: "Late result"})
			if err != nil {
				t.Fatal(err)
			}
			wantTo := ""
			if tc.status == api.AgentDone {
				wantTo = f.lead.ID
			}
			if reply.To != wantTo || reply.ReplyTo != parent.Seq {
				t.Fatalf("reply to %s author: %+v", tc.status, reply)
			}
			peerInbox, err := f.s.ListMessages(f.ctx, f.task.ID, parent.Seq, f.builder.ID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status == api.AgentDone {
				if len(peerInbox) != 0 {
					t.Fatalf("done author's directed reply reached peer: %+v", peerInbox)
				}
			} else if len(peerInbox) != 1 || peerInbox[0].Seq != reply.Seq || peerInbox[0].To != "" {
				t.Fatalf("live peer lost board-wide reply to %s author: %+v", tc.status, peerInbox)
			}
			// An explicit destination is never overridden, even if the
			// replied-to author has already left the project.
			explicit, err := f.post(t, api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, To: f.builder.ID, ReplyTo: parent.Seq, Text: "Directed follow-up"})
			if err != nil || explicit.To != f.builder.ID {
				t.Fatalf("explicit recipient after %s author: %+v %v", tc.status, explicit, err)
			}
		})
	}
}

// wi_229dc7192456ff6e: a directed message whose only recipient is its sender
// is never unread, wakes nobody and obliges nobody, so the hub refuses it with
// a reason the sender can act on.
func TestSelfAddressedMessageIsRefused(t *testing.T) {
	f := newPhase3Fixture(t)
	count := func() int {
		t.Helper()
		var n int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// tt send resolves a named recipient to its id; a role is left to the hub.
	typed := func(from api.Agent, kind, to string) api.PostMessageRequest {
		req := api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID,
			Envelope: &api.Envelope{Kind: kind, To: to, Subject: "The matrix run has finished", Body: api.EnvelopeBody{Text: "Wake me", Ask: "please"}}}
		if !strings.HasPrefix(to, "role:") {
			req.To = from.ID
		}
		return req
	}

	// A plain post, a typed send by name and by id, and a role the sender
	// itself holds are all refused, and nothing is stored.
	before := count()
	for name, req := range map[string]api.PostMessageRequest{
		"plain post to own id":       {AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.builder.ID, Text: "Matrix done"},
		"typed notice to own name":   typed(f.builder, api.EnvelopeKindNotice, f.builder.Name),
		"typed notice to own id":     typed(f.builder, api.EnvelopeKindNotice, f.builder.ID),
		"typed request to own name":  typed(f.builder, api.EnvelopeKindRequest, f.builder.Name),
		"lead to role:lead":          typed(f.lead, api.EnvelopeKindNotice, "role:lead"),
		"handler to its own role":    typed(f.handler, api.EnvelopeKindNotice, "role:database_handler"),
		"handler request to its own": typed(f.handler, api.EnvelopeKindRequest, "role:database_handler"),
	} {
		m, err := f.post(t, req)
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), selfAddressedMessage) {
			t.Errorf("%s: %+v, %v", name, m, err)
		}
	}
	// The reason names what to do instead.
	if !strings.Contains(selfAddressedMessage, "wakes nobody") || !strings.Contains(selfAddressedMessage, "tt event") {
		t.Errorf("reason = %q", selfAddressedMessage)
	}
	var obligations int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID).Scan(&obligations); err != nil {
		t.Fatal(err)
	}
	if after := count(); after != before || obligations != 0 {
		t.Fatalf("refused posts stored %d message(s) and %d obligation(s)", after-before, obligations)
	}

	// A refused post with a request ID stores no receipt: the same key then
	// posts a different, deliverable message.
	keyed := api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.builder.ID, Text: "Matrix done", RequestID: "self-note"}
	if _, err := f.post(t, keyed); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("keyed self post: %v", err)
	}
	keyed.To = f.lead.ID
	if m, err := f.post(t, keyed); err != nil || m.To != f.lead.ID {
		t.Fatalf("key was consumed by the refusal: %+v %v", m, err)
	}
}

// What the refusal must leave alone: other agents as recipients, by name or
// by role; undirected and human posts; and a keyed replay.
func TestSelfAddressedRefusalLeavesOtherPostsAlone(t *testing.T) {
	f := newPhase3Fixture(t)
	unread := func(agent api.Agent, seq int64) bool {
		t.Helper()
		inbox, err := f.s.ListMessages(f.ctx, f.task.ID, seq-1, agent.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range inbox {
			if m.Seq == seq {
				return true
			}
		}
		return false
	}

	// Another agent by id and by typed name.
	direct, err := f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, Text: "Matrix done"})
	if err != nil || direct.To != f.lead.ID || !unread(f.lead, direct.Seq) {
		t.Fatalf("post to another agent: %+v %v", direct, err)
	}
	byName := request(f.builder, f.lead.Name, "Review the fixture plan please")
	byName.To = f.lead.ID
	named, err := f.post(t, byName)
	if err != nil || named.To != f.lead.ID {
		t.Fatalf("typed send to another agent: %+v %v", named, err)
	}
	if o := f.obligationFor(t, named.Seq); o.AgentID != f.lead.ID {
		t.Fatalf("obligation: %+v", o)
	}

	// A role another agent holds still reaches its holder, including the role
	// the refused senders hold themselves when someone else uses it.
	toLead, err := f.post(t, request(f.builder, "role:lead", "Accept the fixture result please"))
	if err != nil || toLead.To != f.lead.ID || !unread(f.lead, toLead.Seq) {
		t.Fatalf("role:lead from the builder: %+v %v", toLead, err)
	}
	if o := f.obligationFor(t, toLead.Seq); o.AgentID != f.lead.ID {
		t.Fatalf("role obligation: %+v", o)
	}
	toHandler, err := f.post(t, request(f.lead, "role:database_handler", "Record the fixture revision please"))
	if err != nil || toHandler.To != f.handler.ID || !unread(f.handler, toHandler.Seq) {
		t.Fatalf("role:database_handler from the lead: %+v %v", toHandler, err)
	}

	// Undirected agent posts, plain and typed.
	board, err := f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, Text: "Team announcement"})
	if err != nil || board.To != "" {
		t.Fatalf("undirected post: %+v %v", board, err)
	}
	notice, err := f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "The fixture build has started", Body: api.EnvelopeBody{Text: "Started"}}})
	if err != nil || notice.To != "" {
		t.Fatalf("undirected typed notice: %+v %v", notice, err)
	}
	// An agent's follow-up to its own message stays an undirected reply.
	followUp, err := f.post(t, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, ReplyTo: board.Seq, Text: "One more detail"})
	if err != nil || followUp.To != "" {
		t.Fatalf("reply to own message: %+v %v", followUp, err)
	}

	// A person's posts, undirected and to an agent.
	human, err := f.post(t, api.PostMessageRequest{Text: "Owner note"})
	if err != nil || human.To != "" {
		t.Fatalf("human post: %+v %v", human, err)
	}
	humanDirect, err := f.post(t, api.PostMessageRequest{To: f.builder.ID, Text: "Owner note for the builder"})
	if err != nil || humanDirect.To != f.builder.ID {
		t.Fatalf("human post to an agent: %+v %v", humanDirect, err)
	}

	// A keyed replay returns the stored message, not a second one.
	keyed := api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, Text: "Keyed note", RequestID: "keyed-note"}
	first, err := f.post(t, keyed)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.post(t, keyed)
	if err != nil || again.Seq != first.Seq || again.PostReceipt == nil {
		t.Fatalf("keyed replay: %+v %v", again, err)
	}
}

// A self-addressed message stored before the refusal existed still replays
// under its key: the receipt answers before the check.
func TestSelfAddressedStoredMessageStillReplays(t *testing.T) {
	f := newPhase3Fixture(t)
	keyed := api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, Text: "Matrix done", RequestID: "old-self-note"}
	stored, err := f.post(t, keyed)
	if err != nil {
		t.Fatal(err)
	}
	// Make the stored row the pre-refusal shape: sender equal to recipient.
	// The receipt's hash covers the request, so the replay asks for that shape.
	keyed.To = f.builder.ID
	if _, err := f.s.db.Exec(`UPDATE messages SET to_agent=? WHERE seq=?`, f.builder.ID, stored.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE message_post_requests SET payload_hash=? WHERE message_seq=?`, requestHash(keyed), stored.Seq); err != nil {
		t.Fatal(err)
	}
	again, err := f.post(t, keyed)
	if err != nil || again.Seq != stored.Seq || again.To != f.builder.ID {
		t.Fatalf("replay of a stored self-addressed message: %+v %v", again, err)
	}
}
