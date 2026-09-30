package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Owner helper conversation (wi_2de2c1273e34473a, docs/discord-helper-chat.md),
// h7: a real in-process hub whose helper is registered through the real
// POST …/owner-helper route, a fake Discord REST API, and Gateway events
// injected through Bridge.Dispatch. No token file, live hub or network.

type helperHarness struct {
	*harness
	ownerHub *api.Client
	channel2 string // the helper channel
	dm       string // the owner's DM channel
	helper   api.Agent

	clockMu sync.Mutex
	clock   time.Time
}

func newHelperHarness(t *testing.T) *helperHarness {
	t.Helper()
	h := &helperHarness{harness: newHarness(t), clock: time.Date(2026, 9, 30, 10, 20, 0, 0, time.UTC)}
	h.channel2 = h.fake.addChannel(discord.ChannelText, "owner-helper")
	h.ownerHub = &api.Client{Base: h.b.cfg.Hub.Base, Token: ownerToken, HTTP: &http.Client{Timeout: 10 * time.Second}}
	h.b = h.newBridge(h.task.ID, h.channel2)
	if err := h.b.startHelper(h.ctx); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.dm, err = h.state.Get(h.ctx, "helper:dm:"+ownerID); err != nil || h.dm == "" {
		t.Fatalf("no owner DM recorded: %q %v", h.dm, err)
	}
	return h
}

// newBridge is a bridge over the harness's hub, fake Discord and state, as
// a restart would build it.
func (h *helperHarness) newBridge(task, channel string) *Bridge {
	h.t.Helper()
	cfg := h.b.cfg
	cfg.HelperTask, cfg.HelperChannel = task, channel
	cfg.Now = func() time.Time {
		h.clockMu.Lock()
		defer h.clockMu.Unlock()
		return h.clock
	}
	b, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	b.botID = h.fake.botID
	return b
}

func (h *helperHarness) advance(d time.Duration) {
	h.clockMu.Lock()
	h.clock = h.clock.Add(d)
	h.clockMu.Unlock()
}

// register makes the owner's session the project's helper, through the
// hub's real registration route.
func (h *helperHarness) register() api.Agent {
	h.t.Helper()
	out, err := h.ownerHub.RegisterOwnerHelper(h.ctx, h.task.ID, api.RegisterOwnerHelperRequest{Host: "owner-mac", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: fmt.Sprintf("ohreg-%d", time.Now().UnixNano())})
	if err != nil || out.Agent == nil || out.Agent.Role != api.AgentRoleOwnerHelper {
		h.t.Fatalf("register helper: %+v %v", out, err)
	}
	h.helper = *out.Agent
	return h.helper
}

// activity reports the helper's execution state, as the relay would.
func (h *helperHarness) activity(state string) {
	h.t.Helper()
	if _, err := h.ownerHub.ReportActivity(h.ctx, h.task.ID, h.helper.ID, api.ActivityReport{RequestID: fmt.Sprintf("act-%d", time.Now().UnixNano()), RunID: h.helper.RunID,
		Activity: api.AgentActivity{State: state, ObservedAt: time.Now()}}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *helperHarness) lifecycle(kind string) {
	h.t.Helper()
	if _, err := h.ownerHub.PostEvent(h.ctx, h.task.ID, api.PostEventRequest{AgentID: h.helper.ID, RunID: h.helper.RunID, Kind: kind}); err != nil {
		h.t.Fatal(err)
	}
}

// gateway delivers a MESSAGE_CREATE through Dispatch and handles it as the
// event loop does.
func (h *helperHarness) gateway(msg discord.Message) {
	h.t.Helper()
	raw, _ := json.Marshal(msg)
	h.b.Dispatch(discord.Dispatch{Type: "MESSAGE_CREATE", Data: raw})
	d := <-h.b.events
	var m discord.Message
	if err := json.Unmarshal(d.Data, &m); err != nil {
		h.t.Fatal(err)
	}
	if err := h.b.handleMessage(h.ctx, m); err != nil {
		h.t.Fatal(err)
	}
}

// replyAs posts on the board as an agent, answering seq.
func (h *helperHarness) replyAs(a api.Agent, replyTo int64, text string) api.Message {
	h.t.Helper()
	return h.post(api.PostMessageRequest{AgentID: a.ID, RunID: a.RunID, ReplyTo: replyTo, Text: text})
}

// flush runs the helper reply pass and drains the outbox.
func (h *helperHarness) flush() {
	h.t.Helper()
	if err := h.b.helperReplies(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	if err := h.b.drainOutbox(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *helperHarness) botIn(channel string) []*fakeMessage { return h.fake.botMessages(channel) }

// helperPosts are board messages the bridge posted for the owner.
func (h *helperHarness) helperPosts() []api.Message {
	var out []api.Message
	for _, m := range h.boardMessages() {
		if m.From.Node == api.BridgeNode {
			out = append(out, m)
		}
	}
	return out
}

func (h *helperHarness) helperObligations() []api.Obligation {
	h.t.Helper()
	all, err := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{}, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	var out []api.Obligation
	for _, o := range all {
		if o.AgentID == h.helper.ID && o.State != api.ObligationClosed {
			out = append(out, o)
		}
	}
	return out
}

func (h *helperHarness) dmInteraction(id, name string, user string, options ...discord.InteractionOption) map[string]json.RawMessage {
	h.t.Helper()
	in := discord.Interaction{ID: id, Type: discord.InteractionCommand, ChannelID: h.dm, User: &discord.User{ID: user}, Token: "token-" + id,
		Context: discord.ContextBotDM, Data: discord.InteractionData{Name: name, Options: options}}
	h.b.handleInteraction(h.ctx, in)
	return h.fake.lastEdit(in.Token)
}

func editEmbeds(edit map[string]json.RawMessage) []discord.Embed {
	var out []discord.Embed
	_ = json.Unmarshal(edit["embeds"], &out)
	return out
}

func firstLine(s string) string { line, _, _ := strings.Cut(s, "\n"); return line }

// a1, a3 (idle): an owner's DM becomes exactly one directed board message
// to the helper with its Discord source, and an open obligation; an idle
// helper gets a ✅ and no note.
func TestHelperOwnerDMReachesTheHelper(t *testing.T) {
	h := newHelperHarness(t)
	h.register()
	msg := h.fake.dmMessage(h.dm, ownerID, "  What's running right now?  ")
	if msg.GuildID != "" {
		t.Fatal("a DM event must carry no guild_id")
	}
	h.gateway(msg)
	posts := h.helperPosts()
	if len(posts) != 1 {
		t.Fatalf("%d helper posts, want 1: %+v", len(posts), posts)
	}
	p := posts[0]
	if p.From.Node != api.BridgeNode || p.To != h.helper.ID || p.Text != "What's running right now?" || p.Source == nil || p.Source.Kind != api.SourceDiscord || p.Source.ID != msg.ID || p.Source.UserID != ownerID {
		t.Fatalf("post = %+v", p)
	}
	if p.PostReceipt == nil || p.PostReceipt.RequestID != "discord-msg-"+msg.ID {
		t.Fatalf("request ID = %+v, want discord-msg-%s", p.PostReceipt, msg.ID)
	}
	if obl := h.helperObligations(); len(obl) != 1 || obl[0].MessageSeq != p.Seq {
		t.Fatalf("helper obligations = %+v, want one for #%d", obl, p.Seq)
	}
	h.flush()
	if got := h.fake.reactionsOn(msg.ID); len(got) != 1 {
		t.Fatalf("reactions = %v, want one ✅", got)
	}
	if bot := h.botIn(h.dm); len(bot) != 0 {
		t.Fatalf("an idle helper got a note: %q", bot[0].Content)
	}
}

// a1, a13, f2: the guild command list is the base list, JSON-identical,
// with the helper off; turning it on only appends /digest. Off also means
// no DM intent, no global commands and no helper handling.
func TestHelperGuildCommandsUnchangedWhenOff(t *testing.T) {
	h := newHarness(t)
	base, _ := json.Marshal(Commands)
	if err := h.b.registerCommands(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.b.registerGlobalCommands(h.ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(h.fake.commands)
	if string(got) != string(base) {
		t.Fatalf("guild commands with the helper off changed:\n%s\nwant\n%s", got, base)
	}
	if h.fake.globalPuts != 0 {
		t.Fatal("global commands were registered with the helper off")
	}
	if GatewayIntents(false)&discord.IntentDirectMessages != 0 || GatewayIntents(true)&discord.IntentDirectMessages == 0 {
		t.Fatal("the DM intent must follow the helper setting")
	}
	if GatewayIntents(false) != discord.IntentGuilds|discord.IntentGuildMessages|discord.IntentMessageContent {
		t.Fatal("intents with the helper off changed")
	}
	// A guild-less message with the helper off is ignored as before.
	dm := h.fake.addChannel(discord.ChannelDM, "")
	if err := h.b.handleMessage(h.ctx, h.fake.dmMessage(dm, ownerID, "hello?")); err != nil {
		t.Fatal(err)
	}
	for _, m := range h.boardMessages() {
		if m.From.Node == api.BridgeNode {
			t.Fatalf("a DM was posted with the helper off: %+v", m)
		}
	}
	if len(h.fake.botMessages(dm)) != 0 {
		t.Fatal("the bridge answered a DM with the helper off")
	}

	on := newHelperHarness(t)
	if err := on.b.registerCommands(on.ctx); err != nil {
		t.Fatal(err)
	}
	if len(on.fake.commands) != len(Commands)+1 || on.fake.commands[len(Commands)].Name != "digest" {
		t.Fatalf("helper-on guild commands = %+v", on.fake.commands)
	}
	prefix, _ := json.Marshal(on.fake.commands[:len(Commands)])
	if string(prefix) != string(base) {
		t.Fatal("an existing guild command changed with the helper on")
	}
}

// a2: the helper's reply goes to the same DM, headed by the helper's name
// and without mentions; other board messages are not routed.
func TestHelperReplyRoutesToTheSameDM(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	msg := h.fake.dmMessage(h.dm, ownerID, "Is the queue moving?")
	h.gateway(msg)
	asked := h.helperPosts()[0]
	h.flush()
	// Not routed: the owner's own post, another agent's reply, a helper post
	// with no reply target and one replying to an unrelated message.
	h.post(api.PostMessageRequest{To: helper.ID, ReplyTo: asked.Seq, Text: "owner on the board"})
	h.replyAs(h.builder, asked.Seq, "builder chiming in")
	h.replyAs(helper, 0, "a broadcast from the helper")
	unrelated := h.post(api.PostMessageRequest{Text: "unrelated"})
	h.replyAs(helper, unrelated.Seq, "answering something else")
	answer := h.replyAs(helper, asked.Seq, "Yes: @everyone entry 83 is running, round 1.")
	h.flush()
	bot := h.botIn(h.dm)
	if len(bot) != 1 {
		t.Fatalf("%d DM messages, want exactly the helper's answer: %+v", len(bot), bot)
	}
	lines := strings.Split(bot[0].Content, "\n")
	if lines[0] != "🤖 **"+helper.Name+"** · owner helper" || strings.HasPrefix(lines[0], "**owner**") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(bot[0].Content, "entry 83 is running") || lines[len(lines)-1] != fmt.Sprintf("-# #%d", answer.Seq) {
		t.Fatalf("content = %q", bot[0].Content)
	}
	if bad := h.fake.sentWithoutAllowedMentions(); len(bad) != 0 {
		t.Fatalf("sent without allowed_mentions.parse=[]: %v", bad)
	}
	// The helper's reply is still mirrored to the project channel as a board line.
	h.cycle()
	mirrored := false
	for _, l := range h.lines() {
		mirrored = mirrored || strings.Contains(l.Content, "entry 83 is running")
	}
	if !mirrored {
		t.Fatal("the helper's reply was not mirrored to the project channel")
	}
}

// a3: busy, offline and missing helpers; every post leaves an open
// obligation for the helper (the relay's usual wake path).
func TestHelperBusyOfflineAndMissing(t *testing.T) {
	h := newHelperHarness(t)
	missing := h.fake.dmMessage(h.dm, ownerID, "anyone home?")
	h.gateway(missing)
	h.flush()
	if n := len(h.helperPosts()); n != 0 {
		t.Fatalf("%d posts with no helper registered, want 0", n)
	}
	if bot := h.botIn(h.dm); len(bot) != 1 || !strings.Contains(bot[0].Content, "No helper session is registered") || !strings.Contains(bot[0].Content, "/status") {
		t.Fatalf("no-helper note = %+v", bot)
	}

	h.register()
	h.activity("working")
	busy := h.fake.dmMessage(h.dm, ownerID, "when you get a moment")
	h.gateway(busy)
	h.flush()
	if bot := h.botIn(h.dm); len(bot) != 2 || !strings.Contains(bot[1].Content, "Received. The helper is busy") {
		t.Fatalf("busy note = %+v", bot)
	}
	if len(h.fake.reactionsOn(busy.ID)) != 0 {
		t.Fatal("a busy helper's message got a ✅ as well as the note")
	}

	h.lifecycle(api.EventExited)
	retired := h.fake.dmMessage(h.dm, ownerID, "has your session ended?")
	h.gateway(retired)
	h.flush()
	if bot := h.botIn(h.dm); len(bot) != 3 || !strings.Contains(bot[2].Content, "offline") || !strings.Contains(bot[2].Content, "/status") {
		t.Fatalf("offline note = %+v", bot)
	}
	if posts := h.helperPosts(); len(posts) != 2 {
		t.Fatalf("%d posts, want the busy and the offline messages", len(posts))
	}
	if obl := h.helperObligations(); len(obl) != 2 {
		t.Fatalf("helper obligations = %+v, want 2 open", obl)
	}
	// Registered but not online (its session is gone), or retired, is offline too.
	if got := classifyHelper(api.Agent{Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning, Online: false}, true); got != helperOffline {
		t.Fatalf("an unseen helper is %s, want offline", got)
	}
	if got := classifyHelper(api.Agent{Role: api.AgentRoleOwnerHelper, Status: api.AgentRetired, Online: true, Activity: &api.AgentActivity{State: "idle"}}, true); got != helperOffline {
		t.Fatalf("a retired helper is %s, want offline", got)
	}
	for state, want := range map[string]helperAvailability{"working": helperBusy, "hung_tool": helperBusy, "looping": helperBusy, "runtime_prompt": helperBusy, "stuck": helperBusy, "idle": helperIdle, "finished_silent": helperIdle, "unknown": helperIdle} {
		a := api.Agent{Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning, Online: true, Activity: &api.AgentActivity{State: state}}
		if got := classifyHelper(a, true); got != want {
			t.Errorf("activity %s = %s, want %s", state, got, want)
		}
	}
}

// a4, f1: a top-level helper-channel message gets its thread at ingest;
// follow-ups chain to the latest helper reply; interleaved threads stay
// apart; the DM chains as one conversation; 160004 counts as success.
func TestHelperThreadContinuity(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	first := h.fake.userMessage(h.channel2, ownerID, "Which items are stuck?", "")
	h.gateway(first)
	if len(h.fake.threadStarts) != 1 || h.fake.threadStarts[0] != first.ID {
		t.Fatalf("thread starts = %v, want one on %s at ingest", h.fake.threadStarts, first.ID)
	}
	second := h.fake.userMessage(h.channel2, ownerID, "Separate question: disk space?", "")
	h.gateway(second)
	posts := h.helperPosts()
	if len(posts) != 2 || posts[0].ReplyTo != 0 || posts[1].ReplyTo != 0 {
		t.Fatalf("top-level posts = %+v", posts)
	}
	r1 := h.replyAs(helper, posts[0].Seq, "Entry 84 waits on the main checkout.")
	r2 := h.replyAs(helper, posts[1].Seq, "The Mini has 31 GiB free.")
	h.flush()
	if bot := h.botIn(first.ID); len(bot) != 1 || !strings.Contains(bot[0].Content, "Entry 84") {
		t.Fatalf("first thread = %+v", bot)
	}
	if bot := h.botIn(second.ID); len(bot) != 1 || !strings.Contains(bot[0].Content, "31 GiB") {
		t.Fatalf("second thread = %+v", bot)
	}
	// Follow-ups in each thread, interleaved.
	h.gateway(h.fake.userMessage(second.ID, ownerID, "and yesterday?", ""))
	h.gateway(h.fake.userMessage(first.ID, ownerID, "and the other one?", ""))
	posts = h.helperPosts()
	if posts[2].ReplyTo != r2.Seq || posts[3].ReplyTo != r1.Seq {
		t.Fatalf("follow-ups reply to %d and %d, want %d and %d", posts[2].ReplyTo, posts[3].ReplyTo, r2.Seq, r1.Seq)
	}
	r3 := h.replyAs(helper, posts[3].Seq, "Entry 85 as well.")
	h.flush()
	if bot := h.botIn(first.ID); len(bot) != 2 || !strings.Contains(bot[1].Content, "Entry 85") || !strings.Contains(bot[1].Content, fmt.Sprintf("-# #%d", r3.Seq)) {
		t.Fatalf("first thread after follow-up = %+v", bot)
	}
	if len(h.botIn(second.ID)) != 1 || len(h.botIn(h.channel2)) != 0 {
		t.Fatal("a reply leaked into the other thread or the channel")
	}
	// The DM is one conversation.
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "DM one"))
	dm1 := h.helperPosts()[4]
	d1 := h.replyAs(helper, dm1.Seq, "DM answer")
	h.flush()
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "DM follow-up"))
	if dm2 := h.helperPosts()[5]; dm2.ReplyTo != d1.Seq {
		t.Fatalf("DM follow-up replies to %d, want %d", dm2.ReplyTo, d1.Seq)
	}
	// A thread start whose answer was lost: the retry's 160004 is success.
	third := h.fake.userMessage(h.channel2, ownerID, "third", "")
	h.fake.failThreads = 1
	h.gateway(third)
	if h.fake.hasChannel(third.ID) {
		t.Fatal("the failed start created the thread")
	}
	third3 := h.helperPosts()[6]
	h.replyAs(helper, third3.Seq, "third answer")
	h.flush()
	if bot := h.botIn(third.ID); len(bot) != 1 {
		t.Fatalf("the thread was not started before the first send: %+v", bot)
	}
	if _, err := h.state.db.ExecContext(h.ctx, `UPDATE helper_conversations SET thread_ready=0 WHERE id=?`, third.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.b.ensureHelperThread(h.ctx, helperLane(third.ID)); err != nil {
		t.Fatalf("160004 must count as started: %v", err)
	}
	if c, _, _ := h.state.conversation(h.ctx, third.ID); !c.ThreadReady {
		t.Fatal("thread not recorded as started after 160004")
	}
}

// a5: a 5,000-character Unicode reply with a code fence arrives as ordered
// parts of at most 2,000 characters, each fence balanced, with markers.
func TestHelperLongReplySplits(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "show me the log"))
	asked := h.helperPosts()[0]
	var text strings.Builder
	text.WriteString("Here it is:\n```go\n")
	for text.Len() < 9000 && utf8.RuneCountInString(text.String()) < 4900 {
		text.WriteString("fmt.Println(\"矩阵🙂 line\")\n")
	}
	text.WriteString("```\nDone.")
	body := text.String()
	if n := utf8.RuneCountInString(body); n < 4900 {
		t.Fatalf("fixture is %d runes", n)
	}
	answer := h.replyAs(helper, asked.Seq, body)
	h.flush()
	bot := h.botIn(h.dm)
	if len(bot) < 3 {
		t.Fatalf("%d parts, want at least 3", len(bot))
	}
	var rebuilt strings.Builder
	for i, m := range bot {
		if n := utf8.RuneCountInString(m.Content); n > contentLimit {
			t.Fatalf("part %d is %d characters", i+1, n)
		}
		lines := strings.Split(m.Content, "\n")
		if want := fmt.Sprintf("-# #%d · part %d/%d", answer.Seq, i+1, len(bot)); lines[len(lines)-1] != want {
			t.Fatalf("part %d marker = %q, want %q", i+1, lines[len(lines)-1], want)
		}
		fences := 0
		for _, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "```") {
				fences++
			}
		}
		if fences%2 != 0 {
			t.Fatalf("part %d has unbalanced fences:\n%s", i+1, m.Content)
		}
		rebuilt.WriteString(m.Content)
	}
	if !strings.HasPrefix(bot[0].Content, "🤖 **"+helper.Name+"**") || !strings.Contains(rebuilt.String(), "Done.") {
		t.Fatal("parts out of order or header missing")
	}
}

// a6: /status in the DM (a global command limited to the bot DM) and in
// the helper channel summarizes hub data, works with the helper absent or
// offline, posts nothing and creates no obligation; a project channel
// keeps its card; a 403 on titles falls back to item IDs.
func TestHelperStatusSummary(t *testing.T) {
	h := newHelperHarness(t)
	for _, c := range globalCommands() {
		if len(c.Contexts) != 1 || c.Contexts[0] != discord.ContextBotDM || len(c.IntegrationTypes) != 1 || c.IntegrationTypes[0] != discord.IntegrationGuildInstall {
			t.Fatalf("global command %s contexts=%v types=%v, want [1] and [0]", c.Name, c.Contexts, c.IntegrationTypes)
		}
	}
	if err := h.b.registerGlobalCommands(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.fake.globalPuts != 1 || len(h.fake.globalCommands) != 2 || h.fake.globalCommands[0].Name != "status" || h.fake.globalCommands[1].Name != "digest" {
		t.Fatalf("global commands = %d puts %+v", h.fake.globalPuts, h.fake.globalCommands)
	}
	if err := h.b.registerGlobalCommands(h.ctx); err != nil || h.fake.globalPuts != 1 {
		t.Fatal("unchanged global commands were registered again")
	}
	item, err := h.st.CreateWorkItem(h.ctx, h.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Discord helper chat", RequestID: "status-item"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	h.askOwner("ask-owner", "Approve the matrix change")
	now := time.Now().UTC()
	h.b.rememberSnapshot(h.task, []api.Agent{h.lead, {ID: "agt_stuck0000000001", Name: "verifier-x", Status: api.AgentRunning, Activity: &api.AgentActivity{State: "hung_tool", LastEventAt: now.Add(-25 * time.Minute)}}},
		mustObligations(t, h), api.TeamQueueList{Entries: []api.TeamQueueEntry{
			{ID: "tqe_1", ItemID: item.ID, Position: 1, State: "running", UpdatedAt: now.Add(-12 * time.Minute).Format(time.RFC3339), Reviews: &api.ReviewConvergence{Rounds: []api.ReviewRound{{Number: 2}}}},
			{ID: "tqe_2", ItemID: "wi_0000000000000002", Position: 2, State: "queued", BlockReason: "All team slots are reserved", Stall: &api.TeamQueueStall{Cause: "idle-entry", Since: now.Add(-40 * time.Minute).Format(time.RFC3339)}},
		}})
	before := len(h.boardMessages())
	obligationsBefore := len(mustObligations(t, h))
	check := func(edit map[string]json.RawMessage, helperLine string, wantTitle bool) {
		t.Helper()
		embeds := editEmbeds(edit)
		if len(embeds) != 1 {
			t.Fatalf("status reply = %s", edit["content"])
		}
		d := embeds[0].Description
		for _, want := range []string{"**Running (1)**", "review round 2", "updated 12m ago", "**Next queued (1)**", "All team slots are reserved", "**Waiting on you (1)**", "Approve the matrix change", "**Stuck (2)**", "idle-entry for 40m", "verifier-x", "hung tool for 25m", "**Helper:** " + helperLine} {
			if !strings.Contains(d, want) {
				t.Errorf("summary lacks %q:\n%s", want, d)
			}
		}
		if strings.Contains(d, "Discord helper chat") != wantTitle || strings.Contains(d, item.ID) == wantTitle {
			t.Errorf("title shown = %v, want %v:\n%s", strings.Contains(d, "Discord helper chat"), wantTitle, d)
		}
	}
	check(h.dmInteraction("920000000000000001", "status", ownerID), "no helper session is registered", true)
	h.register()
	h.lifecycle(api.EventExited)
	check(h.dmInteraction("920000000000000002", "status", ownerID), "owner-helper is offline", true)
	// The helper channel, with titles refused by an older hub.
	h.b.titles = nil
	delete(api.BridgeRoutes, "GET /v1/tasks/{id}/work-items/{wid}")
	t.Cleanup(func() { api.BridgeRoutes["GET /v1/tasks/{id}/work-items/{wid}"] = true })
	in := discord.Interaction{ID: "920000000000000003", Type: discord.InteractionCommand, GuildID: "guild", ChannelID: h.channel2, Member: &discord.Member{User: &discord.User{ID: ownerID}}, Token: "token-status-channel", Data: discord.InteractionData{Name: "status"}}
	h.b.handleInteraction(h.ctx, in)
	check(h.fake.lastEdit(in.Token), "owner-helper is offline", false)
	if len(h.boardMessages()) != before || len(mustObligations(t, h)) != obligationsBefore {
		t.Fatal("/status posted to the board or created an obligation")
	}
	for _, cb := range h.fake.callbacks {
		if cb.Response.Data == nil || cb.Response.Data.Flags != discord.FlagEphemeral {
			t.Fatalf("helper /status reply is not ephemeral: %+v", cb.Response)
		}
	}
	// A project channel's /status is still the project card.
	card := editEmbeds(h.interact(discord.Interaction{ID: "920000000000000004", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "status"}}))
	if len(card) != 1 || !strings.HasPrefix(card[0].Title, "Status · ") {
		t.Fatalf("project /status = %+v", card)
	}
}

// askOwner has the builder send the owner a request, which opens an owner
// obligation.
func (h *helperHarness) askOwner(requestID, subject string) {
	h.t.Helper()
	item, err := h.st.CreateWorkItem(h.ctx, h.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Owner request " + requestID, RequestID: "item-" + requestID}, owner)
	if err != nil {
		h.t.Fatal(err)
	}
	h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, RequestID: requestID, Envelope: &api.Envelope{Kind: "request", To: "owner", Subject: subject, Body: api.EnvelopeBody{Ask: "?"}},
		WorkItems: []api.MessageWorkItem{{ItemTaskID: h.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
}

func mustObligations(t *testing.T, h *helperHarness) []api.Obligation {
	t.Helper()
	all, err := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var open []api.Obligation
	for _, o := range all {
		if o.State != api.ObligationClosed {
			open = append(open, o)
		}
	}
	return open
}

// a7: the digest sends one summary per hour, a line per finished entry or
// new owner request, never what was there before /digest on, never twice
// (not after a restart either), and nothing after /digest off.
func TestHelperDigest(t *testing.T) {
	h := newHelperHarness(t)
	h.askOwner("old-ask", "An old request")
	queue := api.TeamQueueList{Entries: []api.TeamQueueEntry{
		{ID: "tqe_old", ItemID: "wi_0000000000000001", Position: 1, State: "closed"},
		{ID: "tqe_run", ItemID: "wi_0000000000000002", Position: 2, State: "running"},
	}}
	remember := func() { h.b.rememberSnapshot(h.task, nil, mustObligations(t, h), queue) }
	remember()
	edit := h.dmInteraction("930000000000000001", "digest", ownerID, stringOption("state", "on"))
	if !strings.Contains(editContent(edit), "Digest on") {
		t.Fatalf("/digest on = %q", editContent(edit))
	}
	tick := func() []*fakeMessage {
		t.Helper()
		remember()
		if err := h.b.digestTick(h.ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.b.drainOutbox(h.ctx); err != nil {
			t.Fatal(err)
		}
		return h.botIn(h.dm)
	}
	if got := tick(); len(got) != 0 {
		t.Fatalf("history was sent at /digest on: %q", got[0].Content)
	}
	h.advance(time.Hour)
	got := tick()
	if len(got) != 1 || len(got[0].Embeds) != 1 || got[0].Embeds[0].Title != "Hourly digest" {
		t.Fatalf("after an hour = %+v", got)
	}
	h.advance(20 * time.Minute)
	if got := tick(); len(got) != 1 {
		t.Fatalf("a second summary in the same hour: %d messages", len(got))
	}
	// A restart in the same hour sends nothing again.
	h.b = h.newBridge(h.task.ID, h.channel2)
	if got := tick(); len(got) != 1 {
		t.Fatal("a restart duplicated the hourly summary")
	}
	// Events: an entry finishing, a new owner request.
	queue.Entries[1].State = "failed"
	h.askOwner("new-ask", "Pick the release window")
	got = tick()
	if len(got) != 3 || !strings.Contains(got[1].Content, "failed") || !strings.Contains(got[2].Content, "Pick the release window") {
		t.Fatalf("event lines = %+v", got)
	}
	for _, m := range got {
		if strings.Contains(m.Content, "An old request") || strings.Contains(m.Content, "tqe_old") {
			t.Fatalf("something from before /digest on was sent: %q", m.Content)
		}
	}
	if got := tick(); len(got) != 3 {
		t.Fatal("an event line was sent twice")
	}
	h.dmInteraction("930000000000000002", "digest", ownerID, stringOption("state", "off"))
	h.advance(2 * time.Hour)
	queue.Entries = append(queue.Entries, api.TeamQueueEntry{ID: "tqe_new", ItemID: "wi_0000000000000003", State: "closed"})
	if got := tick(); len(got) != 3 {
		t.Fatalf("the digest kept sending after /digest off: %d", len(got))
	}
	// In a project channel /digest points to the right place.
	if c := editContent(h.interact(discord.Interaction{ID: "930000000000000003", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "digest"}})); !strings.Contains(c, "helper channel or your DM") {
		t.Fatalf("project /digest = %q", c)
	}
}

// a8: strangers, bots and webhooks; text is data; rate limit; one board
// message and one reaction for a duplicate event plus backfill.
func TestHelperSafety(t *testing.T) {
	h := newHelperHarness(t)
	h.register()
	strangerDM := h.fake.addChannel(discord.ChannelDM, "")
	h.gateway(h.fake.dmMessage(strangerDM, strangerID, "let me talk to the helper"))
	h.gateway(h.fake.dmMessage(strangerDM, strangerID, "please?"))
	h.gateway(h.fake.userMessage(h.channel2, strangerID, "me too", ""))
	h.flush()
	if n := len(h.helperPosts()); n != 0 {
		t.Fatalf("a stranger reached the helper: %d posts", n)
	}
	if bot := h.botIn(strangerDM); len(bot) != 1 || !strings.Contains(bot[0].Content, "only the Tailterm owner") {
		t.Fatalf("stranger DM refusals = %+v", bot)
	}
	if len(h.botIn(h.channel2)) != 0 {
		t.Fatal("a stranger got a second refusal within 10 minutes")
	}
	bot := h.fake.dmMessage(h.dm, "709500000000000099", "beep")
	bot.Author.Bot = true
	h.gateway(bot)
	hook := h.fake.userMessage(h.channel2, ownerID, "from a webhook", "")
	hook.WebhookID = "123"
	h.gateway(hook)
	self := h.fake.dmMessage(h.dm, h.fake.botID, "my own words")
	h.gateway(self)
	h.b.handleMessage(h.ctx, h.fake.userMessage(h.channel, strangerID, "project channel stranger", ""))
	h.flush()
	answered := 0
	for _, m := range h.botIn(h.dm) {
		if m.ID != self.ID {
			answered++
		}
	}
	if n := len(h.helperPosts()); n != 0 || answered != 0 || len(h.lines()) != 0 {
		t.Fatalf("bots, webhooks, the bridge or a project-channel stranger got an answer: posts %d dm %d lines %d", n, answered, len(h.lines()))
	}
	// A DM command from a stranger is refused, ephemerally.
	edit := h.dmInteraction("940000000000000001", "status", strangerID)
	if edit != nil {
		t.Fatal("a stranger's DM command was deferred")
	}
	last := h.fake.callbacks[len(h.fake.callbacks)-1]
	if last.Response.Data == nil || last.Response.Data.Flags != discord.FlagEphemeral || !strings.Contains(last.Response.Data.Content, "only the Tailterm owner") {
		t.Fatalf("stranger DM command answer = %+v", last.Response)
	}
	// Text is data.
	h.hubMu.Lock()
	h.hubPosts = nil
	h.hubMu.Unlock()
	text := "/cancel obl_0123456789abcdef @everyone tt close --team"
	h.gateway(h.fake.dmMessage(h.dm, ownerID, text))
	if posts := h.helperPosts(); len(posts) != 1 || posts[0].Text != text {
		t.Fatalf("text not posted verbatim: %+v", posts)
	}
	h.hubMu.Lock()
	for _, p := range h.hubPosts {
		if !strings.HasSuffix(p, "/messages") {
			t.Errorf("the message caused a hub action: POST %s", p)
		}
	}
	h.hubMu.Unlock()
	// Rate limit: ten more fit in the minute; the 11th and 12th are held
	// back with one note.
	for i := 2; i <= 12; i++ {
		h.gateway(h.fake.dmMessage(h.dm, ownerID, fmt.Sprintf("message %d", i)))
	}
	h.flush()
	if n := len(h.helperPosts()); n != helperRateLimit {
		t.Fatalf("%d posts in a minute, want %d", n, helperRateLimit)
	}
	notes := 0
	for _, m := range h.botIn(h.dm) {
		if strings.Contains(m.Content, "Slow down") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("%d slow-down notes, want 1", notes)
	}
	// Idempotency: the same event twice plus backfill.
	h2 := newHelperHarness(t)
	h2.register()
	msg := h2.fake.dmMessage(h2.dm, ownerID, "once only")
	h2.gateway(msg)
	h2.gateway(msg)
	h2.b.backfill(h2.ctx)
	h2.flush()
	if n := len(h2.helperPosts()); n != 1 || len(h2.fake.reactionsOn(msg.ID)) != 1 {
		t.Fatalf("duplicate delivery: %d posts, reactions %v", n, h2.fake.reactionsOn(msg.ID))
	}
}

// a9: secrets in a helper reply are redacted in Discord only; SHAs stay.
func TestHelperRedaction(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "check the config"))
	asked := h.helperPosts()[0]
	sha := "8a99f36b92cd1ed16931635d44930a6999a9bdd1"
	digest := "84b4ddbf41c0685498c618ff1ecfc55499cdc63bd3ecd89b058d14bb64bf6dda"
	hexSecret := "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"
	secrets := []string{
		"MTIzNDU2Nzg5MDEyMzQ1Njc4OTA.GAbCdE.abcdefghijklmnopqrstuvwxyz0123456789",
		"sk-ant-api03-abcdefghijklmnop",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"hunter2secret",
		"bearer-value-123",
		// Review round one f5: env-var names and JSON keys.
		"abcDEF123456789xyzQWE",
		"Zq3x_9kLm-token-urlsafe-value",
		"dbpass-4242",
		"openai-abc123def456",
		"wJalrXUtnFEMI-K7MDENG-bPxRfiCY",
		"json-token-abc123",
		// f2: a secret whose value is hex.
		hexSecret,
	}
	text := strings.Join([]string{
		"bot token " + secrets[0],
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----",
		"key " + secrets[1] + " and " + secrets[2],
		"password=" + secrets[3],
		"Authorization: Bearer " + secrets[4],
		"GITHUB_TOKEN=" + secrets[5],
		"export TAILTERM_TOKEN=" + secrets[6],
		"DB_PASSWORD=" + secrets[7],
		"OPENAI_API_KEY=" + secrets[8],
		"AWS_SECRET_ACCESS_KEY=" + secrets[9],
		`{"token": "` + secrets[10] + `", "tokens": 12}`,
		"secret=" + secrets[11],
		"commit " + sha + " digest " + digest + " tokens=1704433706",
	}, "\n")
	reply := h.replyAs(helper, asked.Seq, text)
	h.flush()
	h.cycle() // the project channel mirror (f6)
	got := h.botIn(h.dm)[0].Content
	var mirrored strings.Builder
	for _, l := range h.lines() {
		mirrored.WriteString(l.Content)
		for _, e := range l.Embeds {
			mirrored.WriteString(e.Description)
		}
	}
	if !strings.Contains(mirrored.String(), "commit "+sha) {
		t.Fatalf("the helper's reply was not mirrored to the project channel: %q", mirrored.String())
	}
	for _, s := range append(secrets, "b3BlbnNzaC1rZXktdjEAAAAA") {
		if strings.Contains(got, s) {
			t.Errorf("secret %q reached the DM:\n%s", s, got)
		}
		if strings.Contains(mirrored.String(), s) {
			t.Errorf("secret %q reached the project channel:\n%s", s, mirrored.String())
		}
	}
	if strings.Count(got, redacted) < 13 || !strings.Contains(got, "commit "+sha) || !strings.Contains(got, "digest "+digest) || !strings.Contains(got, "tokens=1704433706") || !strings.Contains(got, `"tokens": 12`) {
		t.Fatalf("redaction = %s", got)
	}
	for _, m := range h.boardMessages() {
		if m.Seq == reply.Seq && m.Text != text {
			t.Fatal("the board text was changed")
		}
	}
	for _, keep := range []string{"tokens=1704433706", "max_tokens: 100", "token_count=5", "the secret sauce", "sk-short", "AKIA", "commit " + sha, "digest: " + digest} {
		if redactSecrets(keep) != keep {
			t.Errorf("%q was redacted", keep)
		}
	}
	if redactSecrets("AKIAABCDEFGHIJKLMNOP xoxb-1234567890-abc") != redacted+" "+redacted {
		t.Errorf("AWS and Slack keys: %q", redactSecrets("AKIAABCDEFGHIJKLMNOP xoxb-1234567890-abc"))
	}
}

// a11: crash recovery and backfill.
func TestHelperRecovery(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	h.activity("working")
	// Killed after the board post, before the note: the claim and the post
	// happened, nothing else. The retry loop finishes it without a duplicate.
	msg := h.fake.dmMessage(h.dm, ownerID, "posted, then the bridge died")
	c, err := h.b.conversationFor(h.ctx, routeDM, msg)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(helperInbound{Route: routeDM, Conversation: c.ID, Message: msg})
	if claimed, err := h.state.ClaimInbound(h.ctx, msg.ID, kindHelperMessage, h.task.ID, string(raw), time.Now().Add(-time.Second)); err != nil || !claimed {
		t.Fatal(err)
	}
	if _, err := h.b.cfg.Hub.PostMessage(h.ctx, h.task.ID, api.PostMessageRequest{To: helper.ID, Text: "posted, then the bridge died", RequestID: "discord-msg-" + msg.ID,
		Source: &api.MessageSource{Kind: api.SourceDiscord, ID: msg.ID, UserID: ownerID}}); err != nil {
		t.Fatal(err)
	}
	h.b = h.newBridge(h.task.ID, h.channel2) // restart
	h.b.retryInbound1(h.ctx)
	h.b.retryInbound1(h.ctx)
	h.gateway(msg) // and a late Gateway redelivery
	h.flush()
	if n := len(h.helperPosts()); n != 1 {
		t.Fatalf("%d board messages after recovery, want 1", n)
	}
	if bot := h.botIn(h.dm); len(bot) != 1 || !strings.Contains(bot[0].Content, "busy") {
		t.Fatalf("notes after recovery = %+v", bot)
	}
	// A reply whose create landed but whose answer was lost is found by its
	// marker, not sent twice.
	h.activity("idle")
	asked := h.helperPosts()[0]
	h.replyAs(helper, asked.Seq, "the answer")
	h.fake.loseCreateMessage = 1
	h.flush()
	h.makeDue()
	h.flush()
	h.flush()
	answers := 0
	for _, m := range h.botIn(h.dm) {
		if strings.Contains(m.Content, "the answer") {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("%d copies of the reply, want 1", answers)
	}
	// Missed while down: DMs and a thread message, read by backfill from
	// Discord-shaped REST data (no guild_id), in order.
	top := h.fake.userMessage(h.channel2, ownerID, "a thread question", "")
	h.gateway(top)
	before := len(h.helperPosts())
	d1 := h.fake.dmMessage(h.dm, ownerID, "missed one")
	d2 := h.fake.dmMessage(h.dm, ownerID, "missed two")
	th := h.fake.userMessage(top.ID, ownerID, "missed in the thread", "")
	h.b.backfill(h.ctx)
	h.b.backfill(h.ctx)
	posts := h.helperPosts()[before:]
	if len(posts) != 3 {
		t.Fatalf("backfill posted %d, want 3: %+v", len(posts), posts)
	}
	for i, want := range []discord.Message{d1, d2, th} {
		p := posts[i]
		if p.Text != want.Content || p.Source == nil || p.Source.ID != want.ID || p.PostReceipt == nil || p.PostReceipt.RequestID != "discord-msg-"+want.ID {
			t.Fatalf("backfilled post %d = %+v, want %q", i, p, want.Content)
		}
	}
}

// Risk 7: a DM that can never be delivered does not hold back project
// mirroring.
func TestHelperLaneIsolation(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "question"))
	asked := h.helperPosts()[0]
	// One reply is attempted with its answer lost, then the DM channel is
	// gone: the retry's reconciliation read fails for good, and so does a
	// new reply's send.
	h.fake.loseCreateMessage = 1
	h.replyAs(helper, asked.Seq, "an answer whose receipt was lost")
	h.flush()
	h.fake.mu.Lock()
	delete(h.fake.channels, h.dm) // the DM channel is gone
	h.fake.mu.Unlock()
	h.makeDue()
	h.replyAs(helper, asked.Seq, "an answer nobody can receive")
	h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Keep mirroring the project"))
	h.flush()
	h.cycle()
	found := false
	for _, l := range h.lines() {
		found = found || strings.Contains(l.Content, "Keep mirroring the project")
	}
	if !found {
		t.Fatal("a failing DM held back the project channel")
	}
	pending, _ := h.state.PendingOutbox(h.ctx, 100)
	for _, r := range pending {
		if r.ChannelID == h.dm {
			t.Fatalf("an undeliverable DM row is still pending: %+v", r)
		}
	}
}

// f3, f7: stale DM commands are cleared when the helper is turned off, and
// a project channel can't be the helper channel.
func TestHelperConfig(t *testing.T) {
	h := newHelperHarness(t)
	if err := h.b.registerGlobalCommands(h.ctx); err != nil || h.fake.globalPuts != 1 {
		t.Fatalf("enable: %v puts %d", err, h.fake.globalPuts)
	}
	off := h.newBridge("", "")
	if err := off.registerGlobalCommands(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.fake.globalPuts != 2 || len(h.fake.globalCommands) != 0 {
		t.Fatalf("disabling left DM commands: puts %d %+v", h.fake.globalPuts, h.fake.globalCommands)
	}
	if err := off.registerGlobalCommands(h.ctx); err != nil || h.fake.globalPuts != 2 {
		t.Fatal("clearing was repeated")
	}
	cfg := h.b.cfg
	cfg.HelperChannel = h.channel // the project's own channel
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), h.task.ID) {
		t.Fatalf("a project channel was accepted as the helper channel: %v", err)
	}
	cfg.HelperTask, cfg.HelperChannel = "", h.channel2
	if _, err := New(cfg); err == nil {
		t.Fatal("a helper channel without a helper project was accepted")
	}
	cfg.HelperTask, cfg.HelperChannel = "not-a-project", ""
	if _, err := New(cfg); err == nil {
		t.Fatal("a bad helper project ID was accepted")
	}
}

// f6: a typed reply (tt send --kind answer --reply-to) renders like the
// project mirror: header with kind and subject, body without the header
// and To lines.
func TestHelperTypedReply(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "what's next?"))
	asked := h.helperPosts()[0]
	m := h.post(api.PostMessageRequest{AgentID: helper.ID, RunID: helper.RunID, ReplyTo: asked.Seq,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Entry 84 launches after 83", Body: api.EnvelopeBody{Text: "It shares the main checkout."}}})
	h.flush()
	bot := h.botIn(h.dm)
	if len(bot) != 1 {
		t.Fatalf("typed reply = %+v", bot)
	}
	lines := strings.Split(bot[0].Content, "\n")
	if lines[0] != "🤖 **"+helper.Name+"** · owner helper · NOTICE · Entry 84 launches after 83" {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(bot[0].Content, "It shares the main checkout.") || strings.Contains(bot[0].Content, firstLine(m.Text)) {
		t.Fatalf("body = %q (board text %q)", bot[0].Content, m.Text)
	}
}

// f0: every Discord message is an obligation the helper must answer or
// acknowledge; answering one of two pending messages works.
func TestHelperReplyToOneOfTwoPending(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "first"))
	h.gateway(h.fake.dmMessage(h.dm, ownerID, "second"))
	posts := h.helperPosts()
	if len(h.helperObligations()) != 2 {
		t.Fatalf("obligations = %+v", h.helperObligations())
	}
	if _, err := h.st.PostMessage(h.ctx, h.task.ID, api.PostMessageRequest{AgentID: helper.ID, RunID: helper.RunID, ReplyTo: posts[1].Seq, Text: "answering the second"}, owner); err != nil {
		t.Fatalf("a reply to one of two pending messages was refused: %v", err)
	}
	h.flush()
	if bot := h.botIn(h.dm); len(bot) != 1 || !strings.Contains(bot[0].Content, "answering the second") {
		t.Fatalf("DM = %+v", bot)
	}
}

// Review round one f6: only an owner helper's messages are redacted in the
// project channel mirror; other agents' messages mirror as before.
func TestHelperMirrorRedactsOnlyTheHelper(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	h.post(api.PostMessageRequest{AgentID: helper.ID, RunID: helper.RunID, Text: "helper says GITHUB_TOKEN=abcDEF123456789xyzQWE"})
	h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, Text: "builder says GITHUB_TOKEN=builderVisible123"})
	h.post(api.PostMessageRequest{AgentID: helper.ID, RunID: helper.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "password=subjectSecret1", Body: api.EnvelopeBody{Text: "api_key=bodySecret22"}}})
	h.cycle()
	var all strings.Builder
	for _, l := range h.lines() {
		all.WriteString(l.Content + "\n")
		for _, e := range l.Embeds {
			all.WriteString(e.Description + "\n")
		}
	}
	for _, leaked := range []string{"abcDEF123456789xyzQWE", "subjectSecret1", "bodySecret22"} {
		if strings.Contains(all.String(), leaked) {
			t.Errorf("%q reached the project channel:\n%s", leaked, all.String())
		}
	}
	if !strings.Contains(all.String(), "GITHUB_TOKEN=builderVisible123") {
		t.Fatalf("another agent's mirrored text changed:\n%s", all.String())
	}
	for _, m := range h.boardMessages() {
		if m.From.AgentID == helper.ID && m.Envelope == nil && !strings.Contains(m.Text, "abcDEF123456789xyzQWE") {
			t.Fatal("the board text was changed")
		}
	}
}

// Review round one f3: free text in /status and the digest is redacted.
func TestHelperSummaryRedactsFreeText(t *testing.T) {
	h := newHelperHarness(t)
	h.askOwner("secret-ask", "Rotate it: GITHUB_TOKEN=summaryLeak123")
	now := time.Now().UTC()
	h.b.rememberSnapshot(h.task, nil, mustObligations(t, h), api.TeamQueueList{Entries: []api.TeamQueueEntry{
		{ID: "tqe_q", ItemID: "wi_0000000000000009", Position: 1, State: "queued", BlockReason: "blocked: password=blockLeak456",
			Stall: &api.TeamQueueStall{Cause: "api_key=stallLeak789", Since: now.Format(time.RFC3339)}},
	}})
	embed, err := h.b.summary(h.ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	events := h.b.digestEvents(h.ctx, []projectSnapshot{{Task: h.task, Obligations: mustObligations(t, h)}})
	var all strings.Builder
	all.WriteString(embed.Description)
	for _, line := range events {
		all.WriteString("\n" + line)
	}
	for _, leaked := range []string{"summaryLeak123", "blockLeak456", "stallLeak789"} {
		if strings.Contains(all.String(), leaked) {
			t.Errorf("%q reached the summary or digest:\n%s", leaked, all.String())
		}
	}
	if !strings.Contains(all.String(), "Rotate it") || !strings.Contains(all.String(), redacted) {
		t.Fatalf("summary = %s", all.String())
	}
}

// Review round one f1: the rate limit follows when the owner wrote, so a
// backfill of messages written minutes apart is not throttled.
func TestHelperRateLimitUsesMessageTime(t *testing.T) {
	h := newHelperHarness(t)
	h.register()
	start := time.Now().Add(-30 * time.Minute)
	if _, err := h.state.db.ExecContext(h.ctx, `UPDATE helper_conversations SET ingest_after=? WHERE id=?`, snowflakeAt(start.Add(-time.Minute)), h.dm); err != nil {
		t.Fatal(err)
	}
	h.fake.mu.Lock()
	for i := 0; i < 12; i++ {
		id := snowflakeAt(start.Add(time.Duration(i) * 2 * time.Minute))
		h.fake.messages[h.dm] = append(h.fake.messages[h.dm], &fakeMessage{Message: discord.Message{ID: id, ChannelID: h.dm, Author: discord.User{ID: ownerID}, Content: fmt.Sprintf("written at minute %d", 2*i)}})
	}
	h.fake.mu.Unlock()
	h.b.backfill(h.ctx)
	h.flush()
	if n := len(h.helperPosts()); n != 12 {
		t.Fatalf("%d of 12 backfilled messages posted", n)
	}
	for _, m := range h.botIn(h.dm) {
		if strings.Contains(m.Content, "Slow down") {
			t.Fatal("a backfill of messages written minutes apart was throttled")
		}
	}
}
