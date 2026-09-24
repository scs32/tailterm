package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// Regression tests for the round-one review of broker phase 2b (C1–C14).

// C9: the splitter always terminates, respects the limit and keeps fences
// balanced, including the reviewer's input that looped forever.
func TestChunkAlwaysProgresses(t *testing.T) {
	check := func(name, text string) {
		t.Helper()
		done := make(chan []string, 1)
		go func() { done <- chunk(text, partBudget) }()
		var parts []string
		select {
		case parts = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: chunk did not terminate", name)
		}
		if len(parts) > 20 {
			t.Fatalf("%s: %d parts for %d runes", name, len(parts), utf8.RuneCountInString(text))
		}
		balanced := strings.Count(text, "```")%2 == 0
		for i, p := range parts {
			if n := utf8.RuneCountInString(p); n > partBudget {
				t.Errorf("%s: part %d has %d runes", name, i+1, n)
			}
			if balanced && strings.Count(p, "```")%2 != 0 {
				t.Errorf("%s: part %d has an unbalanced fence", name, i+1)
			}
		}
	}
	check("reviewer D9", "```"+strings.Repeat("a", 1688)+"\n"+strings.Repeat("b", 2000))
	check("long language tag", "```"+strings.Repeat("x", 3000)+"\ncode\n```")
	check("one huge line", strings.Repeat("é", 8000))
	rng := rand.New(rand.NewPCG(1, 2))
	// Fences only at line starts, as Markdown code blocks are written.
	pieces := []string{"\n```\n", "\n```go\n", "\n", "word ", "ünï ", "🧪", strings.Repeat("z", 700), strings.Repeat("y", 1690)}
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for b.Len() < 8000 {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		check("random", b.String())
	}
}

// C1: a new channel starts from the latest board messages, not from the
// event sequence, which runs far ahead of message numbers.
func TestMirrorStartsFromMessageNumbers(t *testing.T) {
	h := newHarness(t)
	project, err := h.st.CreateTask(h.ctx, api.CreateTaskRequest{Name: "Busy project"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.st.AddAgent(h.ctx, project.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "tt-worker", Runtime: "codex"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ { // events far outnumber messages
		if _, err := h.st.PostEvent(h.ctx, project.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: a.ID, RunID: a.RunID}, owner); err != nil {
			t.Fatal(err)
		}
	}
	var last api.Message
	for i := 0; i < 15; i++ {
		if last, err = h.st.PostMessage(h.ctx, project.ID, api.PostMessageRequest{Text: "history " + itoa(int64(i))}, owner); err != nil {
			t.Fatal(err)
		}
	}
	h.reconcile()
	m, _ := h.state.Mapping(h.ctx, project.ID)
	if m.MirrorAfter != last.Seq-10 {
		t.Fatalf("mirror starts after #%d, want #%d (the newest 10 of %d)", m.MirrorAfter, last.Seq-10, last.Seq)
	}
	next, _ := h.st.PostMessage(h.ctx, project.ID, api.PostMessageRequest{Text: "brand new"}, owner)
	h.cycle()
	found := false
	for _, msg := range h.fake.botMessages(m.ChannelID) {
		found = found || hasLine(msg.Content, marker(next.Seq, 1, 1, ""))
	}
	if !found {
		t.Fatal("a new message was not mirrored")
	}
}

// C2: a send Discord accepted just before the bridge was killed is found by
// its marker after a restart, however much traffic followed and after the
// nonce window has passed.
func TestKilledAfterSendIsFoundNotResent(t *testing.T) {
	h := newHarness(t)
	msg := h.post(api.PostMessageRequest{To: h.lead.ID, Text: "sent just before the crash"})
	if err := h.b.mirrorAll(h.ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.state.PendingOutbox(h.ctx, 10)
	if len(rows) != 1 {
		t.Fatalf("%d pending rows", len(rows))
	}
	// The bridge recorded its attempt, Discord accepted the message, and the
	// process died before recording the result.
	if _, err := h.state.MarkAttempt(h.ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	var p outboxPayload
	_ = json.Unmarshal([]byte(rows[0].Payload), &p)
	h.fake.botSend(h.channel, p.Message.Content)
	for i := 0; i < 250; i++ {
		h.fake.botSend(h.channel, "later traffic "+itoa(int64(i)))
	}
	h.fake.clearNonces()
	restarted, _ := New(h.b.cfg)
	restarted.botID = h.fake.botID
	h.b = restarted
	h.cycle()
	count := 0
	for _, m := range h.fake.botMessages(h.channel) {
		if hasLine(m.Content, marker(msg.Seq, 1, 1, "")) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the message appears %d times after the restart, want 1", count)
	}
	if left, _ := h.state.PendingOutbox(h.ctx, 10); len(left) != 0 {
		t.Fatalf("the row is still pending: %+v", left)
	}
}

// C3: backfill reaches an owner message behind thousands of bot messages,
// and a newer live message never moves the cursor past an unread gap.
func TestBackfillNeverSkipsAGap(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 2100; i++ {
		h.fake.botSend(h.channel, "mirrored "+itoa(int64(i)))
	}
	missed := h.fake.userMessage(h.channel, ownerID, "sent while the gateway was down", "")
	_ = missed
	live := h.fake.userMessage(h.channel, ownerID, "arrived live after the gap", "")
	h.b.handleMessage(h.ctx, live) // the live path must not advance the cursor
	h.b.backfill(h.ctx)
	texts := map[string]int{}
	for _, m := range h.boardMessages() {
		texts[m.Text]++
	}
	if texts["sent while the gateway was down"] != 1 || texts["arrived live after the gap"] != 1 {
		t.Fatalf("board after backfill = %v", texts)
	}
	m, _ := h.state.Mapping(h.ctx, h.task.ID)
	if m.IngestAfter != live.ID {
		t.Fatalf("ingest cursor = %s, want the newest message %s", m.IngestAfter, live.ID)
	}
	// A dropped event is recovered by the next event-loop pass, without a reconnect.
	dropped := h.fake.userMessage(h.channel, ownerID, "dropped from a full queue", "")
	_ = dropped
	h.b.needBackfill.Store(true)
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { h.b.eventLoop(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, bm := range h.boardMessages() {
			found = found || bm.Text == "dropped from a full queue"
		}
		if found {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("a dropped event was not recovered")
}

// C4: an interaction claimed just before a crash is finished afterwards.
func TestInterruptedInteractionIsResumed(t *testing.T) {
	h := newHarness(t)
	in := discord.Interaction{ID: "920000000000000001", Type: discord.InteractionCommand, Token: "tok-resume", GuildID: "guild", ChannelID: h.channel,
		Member: &discord.Member{User: &discord.User{ID: ownerID}},
		Data:   discord.InteractionData{Name: "say", Options: []discord.InteractionOption{stringOption("text", "said before the crash")}}}
	raw, _ := json.Marshal(in)
	claimed, err := h.state.ClaimInbound(h.ctx, "interaction:"+in.ID, "interaction", h.task.ID, string(raw), time.Now().Add(-time.Second))
	if err != nil || !claimed {
		t.Fatal(err)
	}
	h.b.retryInbound1(h.ctx) // the retry loop after a restart
	h.b.retryInbound1(h.ctx) // and a second pass must not repeat it
	count := 0
	for _, m := range h.boardMessages() {
		if m.Text == "said before the crash" {
			count++
		}
	}
	if count != 1 || !strings.Contains(editContent(h.fake.lastEdit("tok-resume")), "said before the crash") {
		t.Fatalf("resumed /say posted %d times; reply %q", count, editContent(h.fake.lastEdit("tok-resume")))
	}
}

// C5: a reply to a bot message whose send was never recorded still reaches
// the agent, found through the message's marker.
func TestReplyToAnUnrecordedSendIsDirected(t *testing.T) {
	h := newHarness(t)
	agentMsg := h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, Text: "Which fixture?"})
	if err := h.b.mirrorAll(h.ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := h.state.PendingOutbox(h.ctx, 10)
	var p outboxPayload
	_ = json.Unmarshal([]byte(rows[0].Payload), &p)
	sent := h.fake.botSend(h.channel, p.Message.Content) // landed, never recorded
	// A forged marker in the owner's own message is not trusted.
	forged := h.fake.userMessage(h.channel, ownerID, "fake\n-# #"+itoa(agentMsg.Seq), "")
	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, ownerID, "reply to my own forged marker", forged.ID))
	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, ownerID, "Use the static fixture", sent))
	msgs := h.boardMessages()
	forgedReply, reply := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if forgedReply.ReplyTo != 0 || forgedReply.To != "" {
		t.Errorf("a forged marker directed the message: %+v", forgedReply)
	}
	if reply.ReplyTo != agentMsg.Seq || reply.To != h.builder.ID {
		t.Fatalf("reply = replyTo %d to %q, want #%d to the builder", reply.ReplyTo, reply.To, agentMsg.Seq)
	}
}

// C6: archiving waits until the project's last messages are mirrored.
func TestArchiveWaitsForTheFinalMirror(t *testing.T) {
	h := newHarness(t)
	h.post(api.PostMessageRequest{To: h.lead.ID, Text: "closing words"})
	if _, err := h.st.CloseTask(h.ctx, h.task.ID, owner); err != nil {
		t.Fatal(err)
	}
	h.hubDown.Store(true)
	tasks := []api.Task{{ID: h.task.ID, Name: h.task.Name, Status: api.TaskClosed}}
	m, _ := h.state.Mapping(h.ctx, h.task.ID)
	if err := h.b.archive(h.ctx, tasks[0], m); err == nil {
		t.Fatal("archive succeeded while the final mirror could not run")
	}
	if m, _ = h.state.Mapping(h.ctx, h.task.ID); m.Archived {
		t.Fatal("the channel was marked archived before its last messages were mirrored")
	}
	h.hubDown.Store(false)
	h.reconcile()
	h.cycle()
	if m, _ = h.state.Mapping(h.ctx, h.task.ID); !m.Archived {
		t.Fatal("not archived once the hub was back")
	}
	found := false
	for _, l := range h.lines() {
		found = found || strings.Contains(l.Content, "closing words")
	}
	if !found {
		t.Fatal("the closing message was never mirrored")
	}
}

// C7: a stuck project does not hold back another project's escalation.
func TestOneProjectCannotStarveAnother(t *testing.T) {
	h := newHarness(t)
	stuck, err := h.st.CreateTask(h.ctx, api.CreateTaskRequest{Name: "Stuck"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	var rows []OutboxRow
	for i := 0; i < 300; i++ {
		rows = append(rows, OutboxRow{Key: "stuck:" + itoa(int64(i)), TaskID: stuck.ID, Kind: kindLine, Payload: payloadJSON(outboxPayload{Message: discord.MessageSend{Content: "x"}})})
	}
	if err := h.state.Enqueue(h.ctx, stuck.ID, 0, rows); err != nil { // no channel: every send waits
		t.Fatal(err)
	}
	later := h.post(api.PostMessageRequest{To: h.lead.ID, Text: "must not wait behind the stuck project"})
	h.cycle()
	found := false
	for _, l := range h.lines() {
		found = found || hasLine(l.Content, marker(later.Seq, 1, 1, ""))
	}
	if !found {
		t.Fatal("the stuck project starved this one")
	}
}

// C8: a fatal Gateway close stops every loop and returns the error.
func TestFatalGatewayCloseStopsTheBridge(t *testing.T) {
	h := newHarness(t)
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Write(context.Background(), websocket.MessageText, []byte(`{"op":10,"d":{"heartbeat_interval":45000}}`))
		_, _, _ = conn.Read(context.Background())
		conn.Close(4014, "Disallowed intent(s)")
	}))
	defer ws.Close()
	url := "ws" + strings.TrimPrefix(ws.URL, "http")
	h.b.cfg.Gateway = &discord.Gateway{Token: "t", URL: func(context.Context) (string, error) { return url, nil }, Log: t.Logf}
	done := make(chan error, 1)
	go func() { done <- h.b.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, discord.ErrFatalClose) {
			t.Fatalf("Run = %v, want ErrFatalClose", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run kept going after a fatal Gateway close")
	}
}

// C14: a failed pin is retried later rather than forgotten.
func TestFailedPinIsRetried(t *testing.T) {
	h := newHarness(t)
	// Start from a project with no card yet.
	if _, err := h.state.db.ExecContext(h.ctx, `UPDATE channels SET card_message_id='',card_hash='',card_pinned=0,card_attempt_at='',card_pin_at='' WHERE task_id=?`, h.task.ID); err != nil {
		t.Fatal(err)
	}
	h.fake.failPins = 1
	m, _ := h.state.Mapping(h.ctx, h.task.ID)
	if err := h.b.refreshCard(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	m, _ = h.state.Mapping(h.ctx, h.task.ID)
	if m.CardMessageID == "" || m.CardPinned {
		t.Fatalf("after a failed pin: %+v", m)
	}
	if err := h.b.refreshCard(h.ctx, m); err != nil { // too soon to retry
		t.Fatal(err)
	}
	if h.fake.pins[m.CardMessageID] {
		t.Fatal("the pin was retried before PinRetry")
	}
	if _, err := h.state.db.ExecContext(h.ctx, `UPDATE channels SET card_pin_at=? WHERE task_id=?`, ts(time.Now().Add(-PinRetry-time.Minute)), h.task.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = h.state.Mapping(h.ctx, h.task.ID)
	if err := h.b.refreshCard(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	if m, _ = h.state.Mapping(h.ctx, h.task.ID); !m.CardPinned || !h.fake.pins[m.CardMessageID] {
		t.Fatalf("the card was not pinned on retry: %+v", m)
	}
}

// Round two (focused fix): R1–R4.

// R1: a message whose intake could not be recorded stops the backfill
// cursor, and is picked up once intake works again.
func TestBackfillStopsAtAnUnrecordedMessage(t *testing.T) {
	h := newHarness(t)
	blocked := h.fake.userMessage(h.channel, ownerID, "could not be recorded at first", "")
	h.fake.userMessage(h.channel, ownerID, "after the failure", "")
	if _, err := h.state.db.ExecContext(h.ctx, `CREATE TRIGGER refuse BEFORE INSERT ON inbound WHEN NEW.source_id='`+blocked.ID+`' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	h.b.backfill(h.ctx)
	m, _ := h.state.Mapping(h.ctx, h.task.ID)
	if !snowflakeAfter(blocked.ID, m.IngestAfter) {
		t.Fatalf("the cursor %s moved past the unrecorded message %s", m.IngestAfter, blocked.ID)
	}
	if _, err := h.state.db.ExecContext(h.ctx, `DROP TRIGGER refuse`); err != nil {
		t.Fatal(err)
	}
	h.b.backfill(h.ctx)
	texts := map[string]int{}
	for _, bm := range h.boardMessages() {
		texts[bm.Text]++
	}
	if texts["could not be recorded at first"] != 1 || texts["after the failure"] != 1 {
		t.Fatalf("board = %v", texts)
	}
}

// R2: a nudge interaction replayed after a crash is not a second nudge.
func TestReplayedNudgeIsNotRepeated(t *testing.T) {
	h := newHarness(t)
	assign := h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Fix the stale smoke test"))
	click := discord.Interaction{ID: "930000000000000001", Type: discord.InteractionComponent, Token: "tok-nudge"}
	obligations, _ := h.b.cfg.Hub.ListObligations(h.ctx, h.task.ID, "", "", true, false)
	for _, o := range obligations {
		if o.MessageSeq == assign.Seq {
			click.Data = discord.InteractionData{CustomID: "nudge:" + o.ID}
		}
	}
	if got := editContent(h.interact(click)); !strings.Contains(got, "Nudged") {
		t.Fatalf("nudge = %q", got)
	}
	// The bridge died before recording that it finished; recovery replays it.
	if _, err := h.state.db.ExecContext(h.ctx, `UPDATE inbound SET state='pending',next_at=? WHERE source_id=?`, ts(time.Now().Add(-time.Second)), "interaction:"+click.ID); err != nil {
		t.Fatal(err)
	}
	h.b.retryInbound1(h.ctx)
	if got := editContent(h.fake.lastEdit("tok-nudge")); !strings.Contains(got, "Nudged") {
		t.Fatalf("replayed nudge = %q; the hub should return the original nudge", got)
	}
	// A different click inside the cooldown is refused, proving the replay
	// did not create a nudge of its own that reset nothing.
	fresh := click
	fresh.ID, fresh.Token = "930000000000000002", "tok-nudge-2"
	if got := editContent(h.interact(fresh)); !strings.Contains(got, "nudged recently") {
		t.Fatalf("a new nudge inside the cooldown = %q", got)
	}
}

// R3: a state database from the first bridge build opens and works.
func TestStateFromTheFirstBuildMigrates(t *testing.T) {
	path := t.TempDir() + "/bridge.sqlite"
	s, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, drop := range []string{"channels DROP COLUMN card_attempt_at", "channels DROP COLUMN card_pinned", "channels DROP COLUMN card_pin_at", "outbox DROP COLUMN first_attempt_at"} {
		if _, err := s.db.Exec("ALTER TABLE " + drop); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO channels (task_id,channel_id) VALUES ('tsk_1','1')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenState(path)
	if err != nil {
		t.Fatalf("reopening a first-build database: %v", err)
	}
	defer s.Close()
	if m, err := s.Mapping(context.Background(), "tsk_1"); err != nil || m.ChannelID != "1" {
		t.Fatalf("mapping after migration = %+v %v", m, err)
	}
	if _, err := s.PendingHeads(context.Background()); err != nil {
		t.Fatalf("outbox after migration: %v", err)
	}
}

// R4: a deleted card is replaced; a settled create leaves no stale window.
func TestDeletedCardIsReplaced(t *testing.T) {
	h := newHarness(t)
	m, _ := h.state.Mapping(h.ctx, h.task.ID)
	if m.CardMessageID == "" || !m.CardAttemptAt.IsZero() {
		t.Fatalf("after creating the card: %+v (the attempt time should be cleared)", m)
	}
	h.fake.mu.Lock()
	msgs := h.fake.messages[h.channel]
	for i, fm := range msgs {
		if fm.ID == m.CardMessageID {
			h.fake.messages[h.channel] = append(msgs[:i:i], msgs[i+1:]...)
		}
	}
	h.fake.mu.Unlock()
	h.post(api.PostMessageRequest{To: h.builder.ID, Text: "changes the card"})
	h.b.cfg.CardInterval = 0
	if err := h.b.refreshCard(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	after, _ := h.state.Mapping(h.ctx, h.task.ID)
	if after.CardMessageID == "" || after.CardMessageID == m.CardMessageID || h.fake.find(h.channel, after.CardMessageID) == nil {
		t.Fatalf("the deleted card was not replaced: %+v", after)
	}
}
