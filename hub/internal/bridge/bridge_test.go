package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Broker phase 2b bridge acceptance d5–d14 (docs/broker-phase-2b.md),
// against a real in-process hub and a fake Discord.

const (
	ownerID     = "709500000000000001"
	strangerID  = "709500000000000002"
	bridgeToken = "bridge-token-0123456789abcdef012345678"
	ownerToken  = "owner-token-0123456789abcdef0123456789"
)

var owner = api.Caller{Node: "workspace", User: "owner"}

type harness struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	hubDown atomic.Bool
	fake    *fakeDiscord
	state   *State
	b       *Bridge
	task    api.Task
	lead    api.Agent
	builder api.Agent
	channel string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background()}
	var err error
	if h.st, err = store.Open(filepath.Join(t.TempDir(), "hub.sqlite")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.st.Close() })
	identity, err := server.TokenIdentities(map[string]api.Caller{ownerToken: owner, bridgeToken: {Node: api.BridgeNode, User: "owner"}})
	if err != nil {
		t.Fatal(err)
	}
	hubHandler := server.New(h.st, identity)
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.hubDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		hubHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(hubSrv.Close)
	hub, err := api.NewClient(hubSrv.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hub.Token = bridgeToken
	h.fake = newFakeDiscord(t)
	if h.state, err = OpenState(filepath.Join(t.TempDir(), "bridge.sqlite")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.state.Close() })
	h.b, err = New(Config{Hub: hub, Discord: h.fake.client(), AppID: "app", GuildID: "guild", Owners: []string{ownerID},
		TailOSURL: "https://tailos.example", State: h.state, CardInterval: time.Hour, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	h.b.botID = h.fake.botID
	if h.task, err = h.st.CreateTask(h.ctx, api.CreateTaskRequest{Name: "Shadow week", Orchestrator: "lead"}, owner); err != nil {
		t.Fatal(err)
	}
	h.lead, h.builder = h.agent("lead"), h.agent("builder-41b1c632")
	h.reconcile()
	m, err := h.state.Mapping(h.ctx, h.task.ID)
	if err != nil || m.ChannelID == "" {
		t.Fatalf("no channel after reconcile: %+v %v", m, err)
	}
	h.channel = m.ChannelID
	return h
}

func (h *harness) agent(name string) api.Agent {
	h.t.Helper()
	a, err := h.st.AddAgent(h.ctx, h.task.ID, api.AddAgentRequest{Name: name, Host: "mini", Session: "tt-" + name, Runtime: "codex"}, owner)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func (h *harness) reconcile() {
	h.t.Helper()
	if err := h.b.reconcileProjects(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// cycle mirrors the board and drains the outbox, as the loops would.
func (h *harness) cycle() {
	h.t.Helper()
	if err := h.b.mirrorAll(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.b.drainOutbox(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

// makeDue makes every pending send due now, as if its backoff had passed.
func (h *harness) makeDue() {
	h.t.Helper()
	rows, err := h.state.PendingOutbox(h.ctx, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := h.state.db.ExecContext(h.ctx, `UPDATE outbox SET next_at=? WHERE id=?`, ts(time.Now().Add(-time.Second)), r.ID); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) post(req api.PostMessageRequest) api.Message {
	h.t.Helper()
	m, err := h.st.PostMessage(h.ctx, h.task.ID, req, owner)
	if err != nil {
		h.t.Fatalf("post: %v", err)
	}
	return m
}

func (h *harness) lines() []*fakeMessage {
	var out []*fakeMessage
	for _, m := range h.fake.botMessages(h.channel) {
		if m.Content != cardMarker {
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) boardMessages() []api.Message {
	h.t.Helper()
	msgs, err := h.st.ListMessages(h.ctx, h.task.ID, 0, "", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	return msgs
}

func (h *harness) interact(in discord.Interaction) map[string]json.RawMessage {
	h.t.Helper()
	if in.GuildID == "" {
		in.GuildID = "guild"
	}
	if in.ChannelID == "" {
		in.ChannelID = h.channel
	}
	if in.Member == nil {
		in.Member = &discord.Member{User: &discord.User{ID: ownerID}}
	}
	if in.Token == "" {
		in.Token = "token-" + in.ID
	}
	h.b.handleInteraction(h.ctx, in)
	return h.fake.lastEdit(in.Token)
}

func editContent(edit map[string]json.RawMessage) string {
	var s string
	_ = json.Unmarshal(edit["content"], &s)
	return s
}

func typed(from api.Agent, to api.Agent, kind, subject string) api.PostMessageRequest {
	env := &api.Envelope{Kind: kind, To: to.Name, Subject: subject}
	switch kind {
	case api.EnvelopeKindAssign:
		env.Body = api.EnvelopeBody{Objective: "fix the smoke test", Owns: []string{"tests/tasks-browser.mjs"}, Acceptance: map[string]string{"a1": "passes"}}
	case api.EnvelopeKindNotice:
		env.Body = api.EnvelopeBody{Text: "fyi"}
	case api.EnvelopeKindQuestion:
		env.Body = api.EnvelopeBody{Question: "Which fixture?"}
	}
	return api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, To: to.ID, Envelope: env}
}

// d5: one channel per project even when the create response is lost.
func TestProvisioningSurvivesALostCreate(t *testing.T) {
	h := newHarness(t)
	second, err := h.st.CreateTask(h.ctx, api.CreateTaskRequest{Name: "Another project!"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	h.fake.loseCreateChannel = 1
	h.reconcile() // the create lands but its response is lost
	h.reconcile() // adopts the channel by its topic marker
	h.reconcile()
	var matches []discord.Channel
	for _, c := range h.fake.textChannels() {
		if strings.Contains(c.Topic, "tailterm:"+second.ID) {
			matches = append(matches, c)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("project has %d channels, want 1: %+v", len(matches), matches)
	}
	m, _ := h.state.Mapping(h.ctx, second.ID)
	if m.ChannelID != matches[0].ID || matches[0].ParentID != h.fake.categoryNamed("Tailterm projects") || matches[0].Name != "another-project" {
		t.Fatalf("mapping %+v channel %+v", m, matches[0])
	}
	if len(h.fake.textChannels()) != 2 {
		t.Fatalf("%d text channels, want 2", len(h.fake.textChannels()))
	}
}

// d6: typed and free-text messages mirror in order; long text splits into
// ordered parts with balanced code fences; nothing is sent without
// allowed_mentions.
func TestMirrorInOrderWithParts(t *testing.T) {
	h := newHarness(t)
	h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Fix the stale smoke test"))
	var long strings.Builder
	long.WriteString("Findings 🧪 with ünïcode\n```go\n")
	for i := 0; utf8.RuneCountInString(long.String()) < 5000; i++ {
		long.WriteString("fmt.Println(\"línea número\", i) // ✓ @everyone\n")
	}
	long.WriteString("```\ndone")
	free := h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, Text: long.String()})
	h.post(api.PostMessageRequest{To: h.lead.ID, Text: "owner: thanks"})
	h.cycle()
	h.cycle() // idempotent

	lines := h.lines()
	if len(lines) < 4 {
		t.Fatalf("%d lines, want the assign, several parts and the owner line", len(lines))
	}
	if !strings.Contains(lines[0].Content, "**lead** → **builder-41b1c632** · ASSIGN · Fix the stale smoke test") || len(lines[0].Embeds) != 1 || !strings.Contains(lines[0].Embeds[0].Description, "Objective: fix the smoke test") {
		t.Fatalf("typed line = %q %+v", lines[0].Content, lines[0].Embeds)
	}
	parts := lines[1 : len(lines)-1]
	var rebuilt strings.Builder
	for i, p := range parts {
		if utf8.RuneCountInString(p.Content) > contentLimit {
			t.Errorf("part %d has %d characters", i+1, utf8.RuneCountInString(p.Content))
		}
		if strings.Count(p.Content, "```")%2 != 0 {
			t.Errorf("part %d has an unbalanced code fence", i+1)
		}
		want := marker(free.Seq, i+1, len(parts), "")
		if !hasLine(p.Content, want) {
			t.Errorf("part %d lacks marker %q", i+1, want)
		}
		rebuilt.WriteString(p.Content)
	}
	if !strings.Contains(rebuilt.String(), "Findings 🧪 with ünïcode") || !strings.Contains(rebuilt.String(), "done") {
		t.Error("parts lost text")
	}
	if !strings.Contains(lines[len(lines)-1].Content, "**owner** → **lead**") {
		t.Errorf("last line = %q", lines[len(lines)-1].Content)
	}
	if bad := h.fake.sentWithoutAllowedMentions(); len(bad) > 0 {
		t.Fatalf("sends without an empty allowed_mentions parse list: %v", bad)
	}
}

// d7: notices change only the status card; card edits are coalesced.
func TestNoticesOnlyUpdateTheCard(t *testing.T) {
	h := newHarness(t)
	h.post(typed(h.lead, h.builder, api.EnvelopeKindNotice, "Integration window closes today"))
	h.cycle()
	if n := len(h.lines()); n != 0 {
		t.Fatalf("a notice mirrored %d lines", n)
	}
	m, _ := h.state.Mapping(h.ctx, h.task.ID)
	if err := h.b.refreshCard(h.ctx, m); err != nil {
		t.Fatal(err)
	}
	m, _ = h.state.Mapping(h.ctx, h.task.ID)
	if m.CardMessageID == "" || !h.fake.pins[m.CardMessageID] {
		t.Fatalf("card not created and pinned: %+v", m)
	}
	cardEdits := func() int {
		n := 0
		for _, r := range h.fake.requests {
			if r.Method == "PATCH" && strings.HasSuffix(r.Path, "/messages/"+m.CardMessageID) {
				n++
			}
		}
		return n
	}
	for i := 0; i < 50; i++ {
		h.post(api.PostMessageRequest{To: h.builder.ID, Text: "burst " + string(rune('a'+i%26))})
		cur, _ := h.state.Mapping(h.ctx, h.task.ID)
		if err := h.b.refreshCard(h.ctx, cur); err != nil {
			t.Fatal(err)
		}
	}
	if n := cardEdits(); n != 0 {
		t.Fatalf("%d card edits inside the coalescing window, want 0", n)
	}
	h.b.cfg.CardInterval = 0
	cur, _ := h.state.Mapping(h.ctx, h.task.ID)
	if err := h.b.refreshCard(h.ctx, cur); err != nil {
		t.Fatal(err)
	}
	if n := cardEdits(); n != 1 {
		t.Fatalf("%d card edits after the window, want 1", n)
	}
	card := h.fake.find(h.channel, m.CardMessageID)
	if !strings.Contains(card.Embeds[0].Description, "**builder-41b1c632** · ") || !strings.Contains(card.Embeds[0].Description, "51 open") {
		t.Fatalf("card = %+v", card.Embeds[0])
	}
}

func (h *harness) escalate(level int) api.Message {
	h.t.Helper()
	open, err := h.st.BrokerOpenObligations(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, o := range open {
		if o.TaskID == h.task.ID && o.AgentID == h.builder.ID && o.Needs != api.ObligationNeedsDelivery {
			if err := h.st.BrokerEscalate(h.ctx, o, level, "not acknowledged", time.Now().UTC()); err != nil {
				h.t.Fatal(err)
			}
			msgs := h.boardMessages()
			return msgs[len(msgs)-1]
		}
	}
	h.t.Fatal("no obligation to escalate")
	return api.Message{}
}

// d8: an owner escalation pings only the owner, with the unstall controls;
// a lead escalation is a plain line.
func TestEscalationsReachTheOwner(t *testing.T) {
	h := newHarness(t)
	h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Fix the stale smoke test"))
	h.escalate(1)
	h.escalate(2)
	h.cycle()
	lines := h.lines()
	if len(lines) != 3 {
		t.Fatalf("%d lines, want assign, lead escalation, owner escalation", len(lines))
	}
	lead, own := lines[1], lines[2]
	if strings.Contains(lead.Content, "<@") || len(lead.AllowedMentions.Users) != 0 || len(lead.Components) != 0 {
		t.Errorf("lead escalation pinged or had controls: %q %+v", lead.Content, lead.AllowedMentions)
	}
	if !strings.HasPrefix(own.Content, "<@"+ownerID+"> ⚠️ **") || !strings.Contains(own.Content, "is overdue") || len(own.AllowedMentions.Users) != 1 || own.AllowedMentions.Users[0] != ownerID || len(own.AllowedMentions.Parse) != 0 {
		t.Fatalf("owner escalation = %q mentions %+v", own.Content, own.AllowedMentions)
	}
	var labels []string
	for _, c := range own.Components[0].Components {
		labels = append(labels, c.Label)
	}
	if strings.Join(labels, ",") != "Nudge,Reassign…,Open in TailOS" || !strings.HasPrefix(own.Components[0].Components[0].CustomID, "nudge:obl_") {
		t.Fatalf("controls = %v %+v", labels, own.Components)
	}
}

// d9: owner messages become human board messages; a reply to an agent's
// mirrored message addresses that agent and obliges it.
func TestOwnerMessagesReachTheBoard(t *testing.T) {
	h := newHarness(t)
	agentMsg := h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, Text: "Which fixture should I use?"})
	h.cycle()
	mirrored := h.lines()[0]

	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, ownerID, "Status please, everyone", ""))
	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, ownerID, "Use the static fixture", mirrored.ID))
	msgs := h.boardMessages()
	plain, reply := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if plain.Text != "Status please, everyone" || plain.To != "" || plain.From.Node != api.BridgeNode || plain.Source == nil || plain.Source.UserID != ownerID {
		t.Fatalf("plain = %+v", plain)
	}
	if reply.To != h.builder.ID || reply.ReplyTo != agentMsg.Seq || reply.From.AgentID != "" {
		t.Fatalf("reply = %+v", reply)
	}
	obligations, _ := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{AgentID: h.builder.ID}, time.Now())
	found := false
	for _, o := range obligations {
		found = found || (o.MessageSeq == reply.Seq && o.SourceKind == "human")
	}
	if !found {
		t.Fatalf("the reply created no human obligation: %+v", obligations)
	}
	if len(h.fake.reactions) != 2 {
		t.Errorf("reactions = %v, want two ✅", h.fake.reactions)
	}
	h.cycle()
	for _, l := range h.lines() {
		if strings.Contains(l.Content, "Status please") {
			t.Error("the owner's own Discord message was mirrored back")
		}
	}
}

// d10: strangers, bots, the bridge itself and unmapped channels create nothing.
func TestIgnoredAuthors(t *testing.T) {
	h := newHarness(t)
	before := len(h.boardMessages())
	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, strangerID, "let me in", ""))
	bot := h.fake.userMessage(h.channel, ownerID, "from a bot", "")
	bot.Author.Bot = true
	h.b.handleMessage(h.ctx, bot)
	hook := h.fake.userMessage(h.channel, ownerID, "from a webhook", "")
	hook.WebhookID = "123"
	h.b.handleMessage(h.ctx, hook)
	self := h.fake.userMessage(h.channel, h.fake.botID, "echo", "")
	h.b.handleMessage(h.ctx, self)
	h.b.handleMessage(h.ctx, h.fake.userMessage("999", ownerID, "unmapped", ""))
	other := h.fake.userMessage(h.channel, ownerID, "another guild", "")
	other.GuildID = "other"
	h.b.handleMessage(h.ctx, other)
	if after := len(h.boardMessages()); after != before {
		t.Fatalf("ignored messages created %d board messages", after-before)
	}
	h.interact(discord.Interaction{ID: "900000000000000001", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "status"}, Member: &discord.Member{User: &discord.User{ID: strangerID}}})
	last := h.fake.callbacks[len(h.fake.callbacks)-1]
	if last.ID != "900000000000000001" || last.Response.Type != discord.ResponseMessage || last.Response.Data.Flags != discord.FlagEphemeral || !strings.Contains(last.Response.Data.Content, "Only the project owner") {
		t.Fatalf("stranger interaction answered %+v", last)
	}
}

// d11: redeliveries do nothing; stale buttons report "already handled".
func TestRedeliveriesAndStaleButtons(t *testing.T) {
	h := newHarness(t)
	msg := h.fake.userMessage(h.channel, ownerID, "once only", "")
	h.b.handleMessage(h.ctx, msg)
	h.b.handleMessage(h.ctx, msg)
	count := 0
	for _, m := range h.boardMessages() {
		if m.Text == "once only" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("a redelivered message posted %d times", count)
	}
	assign := h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Fix the stale smoke test"))
	obligations, _ := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{AgentID: h.builder.ID, OpenOnly: true}, time.Now())
	var oid string
	for _, o := range obligations {
		if o.MessageSeq == assign.Seq {
			oid = o.ID
		}
	}
	click := discord.Interaction{ID: "900000000000000002", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: "nudge:" + oid}}
	if got := editContent(h.interact(click)); !strings.Contains(got, "Nudged") {
		t.Fatalf("nudge = %q", got)
	}
	callbacks := len(h.fake.callbacks)
	h.interact(click) // redelivery
	if len(h.fake.callbacks) != callbacks {
		t.Fatal("a redelivered interaction was answered again")
	}
	// The obligation moves; its old buttons are now stale.
	if _, err := h.st.ReassignObligation(h.ctx, h.task.ID, oid, api.ObligationReassignRequest{ToAgentID: h.lead.ID}, owner); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"nudge:" + oid, "reassign:" + oid} {
		got := editContent(h.interact(discord.Interaction{ID: "90000000000000001" + string(rune('0'+i)), Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: id}}))
		if !strings.Contains(got, "Already handled") {
			t.Errorf("stale %s = %q", id, got)
		}
	}
	menu := discord.Interaction{ID: "900000000000000004", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: "reassignto:" + oid, Values: []string{h.builder.ID}}}
	if got := editContent(h.interact(menu)); !strings.Contains(got, "Already handled") {
		t.Errorf("stale menu = %q", got)
	}
}

// d12: a closed project's channel is archived and refuses input visibly.
func TestClosedProjectsArchive(t *testing.T) {
	h := newHarness(t)
	if _, err := h.st.CloseTask(h.ctx, h.task.ID, owner); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	h.cycle()
	if got := h.fake.channel(h.channel).ParentID; got == "" || got != h.fake.categoryNamed("Tailterm archive") {
		t.Fatalf("channel parent = %q, want the archive category", got)
	}
	lines := h.lines()
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1].Content, "archived") {
		t.Fatalf("no archive notice: %+v", lines)
	}
	before := len(h.boardMessages())
	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, ownerID, "are you there?", ""))
	if len(h.boardMessages()) != before {
		t.Fatal("a message in an archived channel reached the board")
	}
	refused := false
	for _, m := range h.fake.botMessages(h.channel) {
		refused = refused || strings.Contains(m.Content, "This project is closed")
	}
	if !refused {
		t.Fatal("the closed-project rejection was not visible")
	}
	got := h.interact(discord.Interaction{ID: "900000000000000003", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "status"}})
	last := h.fake.callbacks[len(h.fake.callbacks)-1]
	if got != nil || !strings.Contains(last.Response.Data.Content, "closed") {
		t.Fatalf("command in an archived channel: %+v", last)
	}
}

func stringOption(name, value string) discord.InteractionOption {
	raw, _ := json.Marshal(value)
	return discord.InteractionOption{Name: name, Type: discord.OptionString, Value: raw}
}

// d13: the slash commands act through the hub.
func TestSlashCommands(t *testing.T) {
	h := newHarness(t)
	past := time.Now().UTC().Add(-time.Hour)
	h.st.SetClockForTest(func() time.Time { return past })
	assign := h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Fix the stale smoke test"))
	question := h.post(typed(h.builder, h.lead, api.EnvelopeKindQuestion, "Which fixture should the test use"))
	h.st.SetClockForTest(func() time.Time { return time.Now().UTC() })

	status := h.interact(discord.Interaction{ID: "910000000000000001", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "status"}})
	if !strings.Contains(string(status["embeds"]), "builder-41b1c632") {
		t.Fatalf("/status = %s", status["embeds"])
	}
	stalled := editContent(h.interact(discord.Interaction{ID: "910000000000000002", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "stalled"}}))
	overdue, _ := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{OpenOnly: true, Overdue: true}, time.Now())
	if !strings.HasPrefix(stalled, "**2 overdue**") || len(overdue) != 2 || strings.Index(stalled, "#"+itoa(assign.Seq)) > strings.Index(stalled, "#"+itoa(question.Seq)) {
		t.Fatalf("/stalled = %q (hub lists %d overdue)", stalled, len(overdue))
	}
	nudged := editContent(h.interact(discord.Interaction{ID: "910000000000000003", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "nudge", Options: []discord.InteractionOption{stringOption("target", "builder")}}}))
	if !strings.Contains(nudged, "Nudged the recipient of #"+itoa(assign.Seq)) {
		t.Fatalf("/nudge builder = %q", nudged)
	}
	again := editContent(h.interact(discord.Interaction{ID: "910000000000000004", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "nudge", Options: []discord.InteractionOption{stringOption("target", "#"+itoa(assign.Seq))}}}))
	if !strings.Contains(again, "nudged recently") {
		t.Fatalf("second /nudge = %q", again)
	}
	said := h.interact(discord.Interaction{ID: "910000000000000005", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "say", Options: []discord.InteractionOption{stringOption("text", "Please prioritise this"), stringOption("agent", "builder")}}})
	msgs := h.boardMessages()
	last := msgs[len(msgs)-1]
	if last.Text != "Please prioritise this" || last.To != h.builder.ID || last.Source == nil || !strings.Contains(editContent(said), "**owner** → **builder-41b1c632**") {
		t.Fatalf("/say posted %+v, replied %q", last, editContent(said))
	}
	for _, cb := range h.fake.callbacks {
		if cb.ID == "910000000000000005" && cb.Response.Data.Flags != 0 {
			t.Error("/say's reply should be visible in the channel")
		}
	}
	moved := editContent(h.interact(discord.Interaction{ID: "910000000000000006", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "reassign", Options: []discord.InteractionOption{stringOption("obligation", itoa(assign.Seq)), stringOption("agent", "lead")}}}))
	if !strings.Contains(moved, "Reassigned #"+itoa(assign.Seq)) {
		t.Fatalf("/reassign = %q", moved)
	}
	leadOpen, _ := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{AgentID: h.lead.ID, OpenOnly: true}, time.Now())
	if len(leadOpen) != 2 {
		t.Fatalf("lead has %d open obligations after /reassign, want the question and the reissued assign", len(leadOpen))
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.Replace(marker(n, 1, 1, ""), "-# #", "", 1))
}

// d14: crashes, outages and rate limits lose and duplicate nothing.
func TestRecovery(t *testing.T) {
	h := newHarness(t)
	// A send that lands but whose response is lost is found by its marker.
	h.fake.loseCreateMessage = 1
	first := h.post(api.PostMessageRequest{To: h.lead.ID, Text: "first"})
	h.cycle()
	rows, _ := h.state.PendingOutbox(h.ctx, 10)
	if len(rows) != 1 || rows[0].Attempts != 1 {
		t.Fatalf("the uncertain send is not pending retry: %+v", rows)
	}
	// A fresh bridge on the same state (a restart) finishes it.
	restarted, err := New(h.b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.botID = h.fake.botID
	h.b = restarted
	h.makeDue()
	h.cycle()
	matches := 0
	for _, m := range h.lines() {
		if hasLine(m.Content, marker(first.Seq, 1, 1, "")) {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("the uncertain send appears %d times", matches)
	}
	// A 429 is waited out, then the send succeeds once.
	h.fake.rateLimitMessages = 1
	second := h.post(api.PostMessageRequest{To: h.lead.ID, Text: "second"})
	h.cycle()
	found := 0
	for _, m := range h.lines() {
		if hasLine(m.Content, marker(second.Seq, 1, 1, "")) {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("after a 429 the message appears %d times", found)
	}
	// The hub is down: owner input waits and is posted once when it returns.
	h.hubDown.Store(true)
	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, ownerID, "while the hub was down", ""))
	h.hubDown.Store(false)
	pending, _ := h.state.PendingInbound(h.ctx, time.Now().Add(time.Hour))
	if len(pending) != 1 {
		t.Fatalf("%d pending inbound, want 1", len(pending))
	}
	for i := 0; i < 2; i++ {
		for _, in := range pending {
			var msg discord.Message
			_ = json.Unmarshal([]byte(in.Payload), &msg)
			m, _ := h.state.Mapping(h.ctx, in.TaskID)
			h.b.postInbound(h.ctx, m, msg, in.Attempts+1)
		}
	}
	count := 0
	for _, m := range h.boardMessages() {
		if m.Text == "while the hub was down" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the delayed message posted %d times", count)
	}
	// Messages sent while the Gateway was disconnected are backfilled once.
	h.fake.userMessage(h.channel, ownerID, "sent during a gateway outage", "")
	h.b.backfill(h.ctx)
	h.b.backfill(h.ctx)
	count = 0
	for _, m := range h.boardMessages() {
		if m.Text == "sent during a gateway outage" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the backfilled message posted %d times", count)
	}
	if bad := h.fake.sentWithoutAllowedMentions(); len(bad) > 0 {
		t.Fatalf("sends without allowed_mentions: %v", bad)
	}
}

// A deleted channel is recreated and its pending sends follow it.
func TestDeletedChannelIsRecreated(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	delete(h.fake.channels, h.channel)
	h.fake.order = nil
	h.fake.mu.Unlock()
	h.post(api.PostMessageRequest{To: h.lead.ID, Text: "after the deletion"})
	h.cycle()
	h.reconcile()
	h.makeDue()
	h.cycle()
	m, _ := h.state.Mapping(h.ctx, h.task.ID)
	if m.ChannelID == "" || m.ChannelID == h.channel {
		t.Fatalf("channel not recreated: %+v", m)
	}
	h.channel = m.ChannelID
	h.cycle()
	ok := false
	for _, l := range h.lines() {
		ok = ok || strings.Contains(l.Content, "after the deletion")
	}
	if !ok {
		t.Fatal("the pending message did not follow the recreated channel")
	}
}
