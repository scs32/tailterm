package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/store"
)

// wi_88d921fdf8ab454e: /bug and /feature file work items from Discord.

func fileCommand(id, name, text string) discord.Interaction {
	return discord.Interaction{ID: id, Type: discord.InteractionCommand,
		Data: discord.InteractionData{Name: name, Options: []discord.InteractionOption{stringOption("text", text)}}}
}

func (h *harness) items() []api.WorkItem {
	h.t.Helper()
	list, err := h.st.ListWorkItems(h.ctx, h.task.ID, "", "", 0, 100)
	if err != nil {
		h.t.Fatal(err)
	}
	return list.Items
}

func (h *harness) openObligations() int {
	h.t.Helper()
	open, err := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{OpenOnly: true}, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return len(open)
}

func (h *harness) queueEntries() int {
	h.t.Helper()
	q, err := h.st.ListQueue(h.ctx, h.task.ID, "", api.MaxQueuePage, true)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(q.Entries)
}

// filingPosts are the hub POSTs that post an intake message or file an item.
func (h *harness) filingPosts() []string {
	h.hubMu.Lock()
	defer h.hubMu.Unlock()
	var out []string
	for _, p := range h.hubPosts {
		if strings.HasSuffix(p, "/messages") || strings.HasSuffix(p, "/work-items") {
			out = append(out, p)
		}
	}
	return out
}

func (h *harness) intakeFor(interactionID string) []api.Message {
	var out []api.Message
	for _, m := range h.boardMessages() {
		if m.Source != nil && m.Source.ID == interactionID {
			out = append(out, m)
		}
	}
	return out
}

// filedPosts are the bot's public announcements of a filed item.
func (h *harness) filedPosts() []*fakeMessage {
	var out []*fakeMessage
	for _, m := range h.fake.botMessages(h.channel) {
		if strings.Contains(m.Content, "Filed bug **") || strings.Contains(m.Content, "Filed feature **") {
			out = append(out, m)
		}
	}
	return out
}

// filedPostsFor are the announcements naming one item.
func (h *harness) filedPostsFor(itemID string) []*fakeMessage {
	var out []*fakeMessage
	for _, m := range h.filedPosts() {
		if strings.Contains(m.Content, "**"+itemID+"**") {
			out = append(out, m)
		}
	}
	return out
}

func deferFlags(t *testing.T, f *fakeDiscord, interactionID string) int {
	t.Helper()
	for _, c := range f.callbacks {
		if c.ID == interactionID && c.Response.Type == discord.ResponseDeferredMessage {
			if c.Response.Data == nil {
				return 0
			}
			return c.Response.Data.Flags
		}
	}
	t.Fatalf("interaction %s was never deferred", interactionID)
	return -1
}

// a1 (d1)
func TestBugFeatureCommandsRegistered(t *testing.T) {
	h := newHarness(t)
	if err := h.b.registerCommands(h.ctx); err != nil {
		t.Fatal(err)
	}
	byName := map[string]discord.Command{}
	for _, c := range h.fake.commands {
		byName[c.Name] = c
		if utf8.RuneCountInString(c.Description) > 100 {
			t.Errorf("/%s description is %d chars", c.Name, utf8.RuneCountInString(c.Description))
		}
		for _, o := range c.Options {
			if utf8.RuneCountInString(o.Description) > 100 {
				t.Errorf("/%s %s description is %d chars", c.Name, o.Name, utf8.RuneCountInString(o.Description))
			}
		}
	}
	for _, name := range []string{"bug", "feature"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("/%s is not registered", name)
		}
		if len(c.Options) != 1 || c.Options[0].Name != "text" || c.Options[0].Type != discord.OptionString || !c.Options[0].Required {
			t.Errorf("/%s options = %+v, want one required string option text", name, c.Options)
		}
	}
	for _, name := range []string{"status", "stalled", "nudge", "say", "reassign", "extend", "answer", "cancel", "resume"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("/%s is no longer registered", name)
		}
	}
}

// a2 (d3, bug)
func TestBugFilesAnItem(t *testing.T) {
	h := newHarness(t)
	obligations, queued := h.openObligations(), h.queueEntries()
	in := fileCommand("950000000000000001", "bug", "Login button is dead. Clicking it on iOS does nothing.")
	got := editContent(h.interact(in))
	items := h.items()
	if len(items) != 1 {
		t.Fatalf("filed %d items, want 1; reply %q", len(items), got)
	}
	it := items[0]
	if it.Kind != "bug" || it.Status != "open" || it.Priority != "normal" || it.Title != "Login button is dead" || it.Description != "Clicking it on iOS does nothing." {
		t.Errorf("item = %s %s %s %q %q", it.Kind, it.Status, it.Priority, it.Title, it.Description)
	}
	if it.CreatedBy.Node != api.BridgeNode || it.CreatedBy.User != "owner" || it.CreatedBy.AgentID != "" {
		t.Errorf("createdBy = %+v, want the owner through the bridge", it.CreatedBy)
	}
	intake := h.intakeFor(in.ID)
	if len(intake) != 1 {
		t.Fatalf("%d intake messages for the interaction, want 1", len(intake))
	}
	m := intake[0]
	if it.SourceMessageSeq != m.Seq || m.From.AgentID != "" || m.From.User != "owner" || m.To != "" ||
		m.Source.Kind != api.SourceDiscord || m.Source.UserID != ownerID ||
		!strings.HasPrefix(m.Text, "Owner intake from Discord (/bug): Login button is dead.") {
		t.Errorf("source message #%d = %+v, item source #%d", m.Seq, m, it.SourceMessageSeq)
	}
	if !strings.Contains(got, "Filed bug **"+it.ID+"**") || !strings.Contains(got, "https://tailos.example/#feature-history/"+h.task.ID+"/"+it.ID) {
		t.Errorf("reply = %q", got)
	}
	if flags := deferFlags(t, h.fake, in.ID); flags != discord.FlagEphemeral {
		t.Errorf("defer flags = %d, want an ephemeral reply", flags)
	}
	posts := h.filedPosts()
	if len(posts) != 1 || !strings.Contains(posts[0].Content, "Filed bug **"+it.ID+"**") ||
		!strings.Contains(posts[0].Content, "https://tailos.example/#feature-history/"+h.task.ID+"/"+it.ID) ||
		posts[0].AllowedMentions == nil || posts[0].AllowedMentions.Parse == nil || len(posts[0].AllowedMentions.Parse) != 0 {
		t.Errorf("channel posts = %+v, want one public announcement of %s without mentions", posts, it.ID)
	}
	if n := h.queueEntries(); n != queued {
		t.Errorf("filing queued the item: %d queue entries, was %d", n, queued)
	}
	if n := h.openObligations(); n != obligations {
		t.Errorf("filing opened obligations: %d, was %d", n, obligations)
	}
}

// a3 (d3, feature)
func TestFeatureFilesAnItem(t *testing.T) {
	h := newHarness(t)
	in := fileCommand("950000000000000002", "feature", "Dark mode for the board\nFollow the system setting.\nKeep the compact layout.")
	got := editContent(h.interact(in))
	items := h.items()
	if len(items) != 1 {
		t.Fatalf("filed %d items, want 1; reply %q", len(items), got)
	}
	it := items[0]
	if it.Kind != "feature" || it.Title != "Dark mode for the board" || it.Description != "Follow the system setting.\nKeep the compact layout." {
		t.Errorf("item = %s %q %q", it.Kind, it.Title, it.Description)
	}
	intake := h.intakeFor(in.ID)
	if len(intake) != 1 || it.SourceMessageSeq != intake[0].Seq || !strings.HasPrefix(intake[0].Text, "Owner intake from Discord (/feature): ") {
		t.Errorf("intake = %+v, item source #%d", intake, it.SourceMessageSeq)
	}
	if !strings.Contains(got, "Filed feature **"+it.ID+"**") || !strings.Contains(got, "/#feature-history/"+h.task.ID+"/"+it.ID) {
		t.Errorf("reply = %q", got)
	}
	if flags := deferFlags(t, h.fake, in.ID); flags != discord.FlagEphemeral {
		t.Errorf("defer flags = %d, want an ephemeral reply", flags)
	}
	if posts := h.filedPostsFor(it.ID); len(posts) != 1 || len(h.filedPosts()) != 1 {
		t.Errorf("%d announcements of %s, %d in all; want one", len(posts), it.ID, len(h.filedPosts()))
	}
}

// wi_747cfb98118d284f f1a, f1b: control characters the hub refuses become
// spaces, so the text files normally; newlines and tabs are kept.
func TestBugFeatureFileControlCharacters(t *testing.T) {
	h := newHarness(t)
	text := "Login\x1b[31m button\x00 is dead.\x07 Clicking\r it does\tnothing.\x7f\x0b\nSecond line\x0c here.\n\tThird\x1f line."
	for i, kind := range []string{"bug", "feature"} {
		in := fileCommand("95000000000000050"+itoa(int64(i+1)), kind, text)
		got := editContent(h.interact(in))
		var filed []api.WorkItem
		for _, it := range h.items() {
			if it.Kind == kind {
				filed = append(filed, it)
			}
		}
		if len(filed) != 1 {
			t.Fatalf("/%s filed %d items, want 1; reply %q", kind, len(filed), got)
		}
		it := filed[0]
		intake := h.intakeFor(in.ID)
		if len(intake) != 1 || it.SourceMessageSeq != intake[0].Seq {
			t.Fatalf("/%s: %d intake messages, item source #%d", kind, len(intake), it.SourceMessageSeq)
		}
		for field, s := range map[string]string{"intake": intake[0].Text, "title": it.Title, "description": it.Description} {
			if !api.ValidText(s, api.MaxTextLen) || strings.ContainsAny(s, "\x00\x07\x0b\x0c\x1b\x7f\r") {
				t.Errorf("/%s %s %q still carries control characters", kind, field, s)
			}
		}
		if it.Title != "Login [31m button is dead. Clicking it does nothing." {
			t.Errorf("/%s title = %q", kind, it.Title)
		}
		if it.Description != "Second line  here.\n\tThird  line." || !strings.Contains(intake[0].Text, "it does\tnothing.") {
			t.Errorf("/%s description lost a newline or tab: %q", kind, it.Description)
		}
		name := "Filed " + kind + " **" + it.ID + "**"
		if !strings.Contains(got, name) || strings.Contains(got, "Nothing was filed") {
			t.Errorf("/%s reply = %q, want it to name %s", kind, got, it.ID)
		}
		if posts := h.filedPostsFor(it.ID); len(posts) != 1 || !strings.Contains(posts[0].Content, name) {
			t.Errorf("/%s: %d channel announcements of %s, want 1", kind, len(posts), it.ID)
		}
	}
}

// a4: title and description rules.
func TestSplitTitle(t *testing.T) {
	long := strings.Repeat("word ", 60) + "end" // 303 runes, one sentence
	for _, c := range []struct{ name, in, title, desc string }{
		{"first line", "Title here\nline two\nline three", "Title here", "line two\nline three"},
		{"crlf", "Title here\r\nmore", "Title here", "more"},
		{"first sentence, dot dropped", "Login button is dead. Clicking it does nothing.", "Login button is dead", "Clicking it does nothing."},
		{"question mark kept", "Why is it slow? It takes a minute.", "Why is it slow?", "It takes a minute."},
		{"exclamation kept", "It crashed! Every time.", "It crashed!", "Every time."},
		{"dot not followed by space", "v1.2 is slow", "v1.2 is slow", ""},
		{"no terminator", "Add a dark mode", "Add a dark mode", ""},
		{"trailing dot only", "Add a dark mode.", "Add a dark mode.", ""},
		{"control chars", "Bad\x01\x02 title\tthere. rest", "Bad title there", "rest"},
		{"dot then text", ". x", ". x", ". x"},
		{"control-only first line", "\x01\x02\nreal title", "real title", "\x01\x02\nreal title"},
		{"control-only text", "\x01\x02", "", ""},
		{"whitespace only", "  \n\t ", "", ""},
		{"abbreviation splits early", "e.g. the board", "e.g", "the board"},
	} {
		title, desc := splitTitle(c.in)
		if title != c.title || desc != c.desc {
			t.Errorf("%s: splitTitle(%q) = %q, %q; want %q, %q", c.name, c.in, title, desc, c.title, c.desc)
		}
		if title != "" && !api.ValidTaskName(title) {
			t.Errorf("%s: title %q is not a valid hub title", c.name, title)
		}
	}
	title, desc := splitTitle(long)
	if n := utf8.RuneCountInString(title); n > 120 || !strings.HasSuffix(title, "…") || desc != long || !api.ValidTaskName(title) {
		t.Errorf("long: title %q (%d runes), description kept %v", title, n, desc == long)
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(title, "…")) || strings.HasSuffix(strings.TrimSuffix(title, "…"), " ") {
		t.Errorf("long title not cut at a word: %q", title)
	}
	unbroken := strings.Repeat("x", 300)
	if title, desc := splitTitle(unbroken); utf8.RuneCountInString(title) != 120 || desc != unbroken {
		t.Errorf("unbroken long title = %d runes, description kept %v", utf8.RuneCountInString(title), desc == unbroken)
	}
}

// a5 (d2)
func TestBugFeatureRefuseNonOwners(t *testing.T) {
	h := newHarness(t)
	messages, items := len(h.boardMessages()), len(h.items())
	for i, c := range []struct{ name, user, guild string }{
		{"bug", strangerID, "guild"}, {"feature", strangerID, "guild"},
		{"bug", ownerID, "other"}, {"feature", ownerID, "other"},
	} {
		in := fileCommand("95000000000000010"+itoa(int64(i)), c.name, "Should not be filed. At all.")
		in.GuildID, in.Member = c.guild, &discord.Member{User: &discord.User{ID: c.user}}
		h.interact(in)
		last := h.fake.callbacks[len(h.fake.callbacks)-1]
		if last.ID != in.ID || last.Response.Type != discord.ResponseMessage || last.Response.Data.Flags != discord.FlagEphemeral || !strings.Contains(last.Response.Data.Content, "Only the project owner") {
			t.Errorf("/%s by %s in %s answered %+v", c.name, c.user, c.guild, last)
		}
		for _, cb := range h.fake.callbacks {
			if cb.ID == in.ID && cb.Response.Type == discord.ResponseDeferredMessage {
				t.Errorf("/%s by %s in %s was deferred", c.name, c.user, c.guild)
			}
		}
	}
	if posts := h.filedPosts(); len(posts) != 0 {
		t.Errorf("refused commands posted %d announcements", len(posts))
	}
	if len(h.boardMessages()) != messages || len(h.items()) != items || len(h.filingPosts()) != 0 {
		t.Fatalf("refused commands changed the hub: messages %d→%d, items %d→%d, posts %v", messages, len(h.boardMessages()), items, len(h.items()), h.filingPosts())
	}
}

// a6 (d5): nothing to file makes no hub call.
func TestBugFeatureRefuseEmptyText(t *testing.T) {
	h := newHarness(t)
	messages := len(h.boardMessages())
	i := 0
	for _, name := range []string{"bug", "feature"} {
		for _, text := range []string{"   \n\t ", "\x01\x02", "\x00\x1b\x7f", "\r\x0b\x0c\x1f"} {
			i++
			in := fileCommand("95000000000000020"+itoa(int64(i)), name, text)
			got := editContent(h.interact(in))
			if !strings.Contains(got, "Nothing was filed") || !strings.Contains(got, "/"+name+" <title>") {
				t.Errorf("/%s %q = %q", name, text, got)
			}
			if flags := deferFlags(t, h.fake, in.ID); flags != discord.FlagEphemeral {
				t.Errorf("/%s %q defer flags = %d, want ephemeral", name, text, flags)
			}
		}
	}
	if posts := h.filedPosts(); len(posts) != 0 {
		t.Errorf("empty commands posted %d announcements", len(posts))
	}
	if len(h.boardMessages()) != messages || len(h.items()) != 0 || len(h.filingPosts()) != 0 {
		t.Fatalf("empty commands changed the hub: messages %d→%d, items %d, posts %v", messages, len(h.boardMessages()), len(h.items()), h.filingPosts())
	}
}

// a7 (d3/d5): a hub error is reported, never dropped.
func TestBugReportsHubErrors(t *testing.T) {
	h := newHarness(t)
	h.hubDown.Store(true)
	got := editContent(h.interact(fileCommand("950000000000000301", "bug", "Hub is down. Still report it.")))
	h.hubDown.Store(false)
	if !strings.HasPrefix(got, "⛔ Tailterm refused this bug") || !strings.Contains(got, "Nothing was filed") {
		t.Errorf("hub down reply = %q", got)
	}
	if len(h.items()) != 0 {
		t.Fatal("an item was filed while the hub was down")
	}

	h.hubFail.Store(&hubFault{Route: "POST /work-items"})
	in := fileCommand("950000000000000302", "feature", "Items are refused. Messages are not.")
	got = editContent(h.interact(in))
	intake := h.intakeFor(in.ID)
	if len(intake) != 1 {
		t.Fatalf("%d intake messages, want 1; reply %q", len(intake), got)
	}
	if !strings.HasPrefix(got, "⚠️ Your text was saved as board message #"+itoa(intake[0].Seq)+",") || !strings.Contains(got, "refused the feature") {
		t.Errorf("work-items fault reply = %q", got)
	}
	if len(h.items()) != 0 {
		t.Fatal("an item was filed while work items were refused")
	}
}

// a8 (d4): a retried interaction files exactly once.
func TestBugRetryIsIdempotent(t *testing.T) {
	h := newHarness(t)
	// (i) the item failed after the intake message was saved; the same
	// interaction, run again, reuses that message and files one item.
	h.hubFail.Store(&hubFault{Route: "POST /work-items"})
	in := fileCommand("950000000000000401", "bug", "Retry me. Once only.")
	in.GuildID, in.ChannelID, in.Token = "guild", h.channel, "tok-401"
	in.Member = &discord.Member{User: &discord.User{ID: ownerID}}
	h.b.handleInteraction(h.ctx, in)
	h.hubFail.Store(nil)
	r := h.b.act(h.ctx, h.task.ID, in)
	intake, items := h.intakeFor(in.ID), h.items()
	if len(intake) != 1 || len(items) != 1 || items[0].SourceMessageSeq != intake[0].Seq || !strings.Contains(r.Content, items[0].ID) {
		t.Fatalf("retry after a fault: %d intake messages, %d items, reply %q", len(intake), len(items), r.Content)
	}
	if again := h.b.act(h.ctx, h.task.ID, in); !strings.Contains(again.Content, items[0].ID) || len(h.items()) != 1 || len(h.intakeFor(in.ID)) != 1 {
		t.Fatalf("a second replay filed again: reply %q, %d items", again.Content, len(h.items()))
	}
	// Without the nonce, the earlier announcement is still found by its marker.
	h.fake.clearNonces()
	if again := h.b.act(h.ctx, h.task.ID, in); !strings.Contains(again.Content, items[0].ID) {
		t.Fatalf("a replay after a nonce reset: reply %q", again.Content)
	}
	if posts := h.filedPostsFor(items[0].ID); len(posts) != 1 {
		t.Fatalf("%d announcements of %s after replays, want 1", len(posts), items[0].ID)
	}

	// (ii) a claim left by a crash is resumed by the retry loop, twice.
	crashed := fileCommand("950000000000000402", "feature", "Filed before the crash. Resumed after it.")
	crashed.GuildID, crashed.ChannelID, crashed.Token = "guild", h.channel, "tok-402"
	crashed.Member = &discord.Member{User: &discord.User{ID: ownerID}}
	raw, _ := json.Marshal(crashed)
	claimed, err := h.state.ClaimInbound(h.ctx, "interaction:"+crashed.ID, "interaction", h.task.ID, string(raw), time.Now().Add(-time.Second))
	if err != nil || !claimed {
		t.Fatal(err)
	}
	h.b.retryInbound1(h.ctx)
	h.b.retryInbound1(h.ctx)
	intake = h.intakeFor(crashed.ID)
	var filed []api.WorkItem
	for _, it := range h.items() {
		if it.Kind == "feature" {
			filed = append(filed, it)
		}
	}
	if len(intake) != 1 || len(filed) != 1 || filed[0].SourceMessageSeq != intake[0].Seq {
		t.Fatalf("resumed /feature: %d intake messages, %d items", len(intake), len(filed))
	}
	if got := editContent(h.fake.lastEdit("tok-402")); !strings.Contains(got, filed[0].ID) {
		t.Fatalf("resumed reply = %q, want it to name %s", got, filed[0].ID)
	}
	if posts := h.filedPostsFor(filed[0].ID); len(posts) != 1 {
		t.Fatalf("%d announcements of resumed %s, want 1", len(posts), filed[0].ID)
	}
}

// wi_747cfb98118d284f f2a, f2b: every /bug and /feature reply is ephemeral,
// and only a success is announced in the channel.
func TestBugFeatureRepliesPerOutcome(t *testing.T) {
	h := newHarness(t)
	for i, c := range []struct {
		name, kind, text string
		setup            func()
		prefix           string
		posts            int
	}{
		{"success", "bug", "Works fine. Files.", nil, "🐞 Filed bug **", 1},
		{"usage", "feature", "  ", nil, "Describe the feature", 0},
		{"intake refused", "bug", "Refused intake. Body.", func() { h.hubFail.Store(&hubFault{Route: "POST /messages"}) }, "⛔ Tailterm refused this bug", 0},
		{"hub down", "feature", "Hub down. Body.", func() { h.hubDown.Store(true) }, "⛔ Tailterm refused this feature", 0},
		{"item refused", "bug", "Refused item. Body.", func() { h.hubFail.Store(&hubFault{Route: "POST /work-items"}) }, "⚠️ Your text was saved", 0},
	} {
		before := len(h.fake.botMessages(h.channel))
		if c.setup != nil {
			c.setup()
		}
		in := fileCommand("95000000000000060"+itoa(int64(i+1)), c.kind, c.text)
		got := editContent(h.interact(in))
		h.hubFail.Store(nil)
		h.hubDown.Store(false)
		if !strings.HasPrefix(got, c.prefix) {
			t.Errorf("%s: reply = %q, want prefix %q", c.name, got, c.prefix)
		}
		if flags := deferFlags(t, h.fake, in.ID); flags != discord.FlagEphemeral {
			t.Errorf("%s: defer flags = %d, want ephemeral", c.name, flags)
		}
		if n := len(h.fake.botMessages(h.channel)) - before; n != c.posts {
			t.Errorf("%s: %d channel posts, want %d", c.name, n, c.posts)
		}
	}
	posts := h.filedPosts()
	if len(posts) != 1 || posts[0].AllowedMentions == nil || posts[0].AllowedMentions.Parse == nil || len(posts[0].AllowedMentions.Parse) != 0 {
		t.Errorf("announcements = %+v, want one without mentions", posts)
	}
}

// wi_747cfb98118d284f: a failed announcement still files once and tells the
// owner in the ephemeral reply.
func TestBugAnnounceFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.fake.failMessages = 1
	in := fileCommand("950000000000000701", "bug", "Channel post fails. Item still files.")
	got := editContent(h.interact(in))
	items := h.items()
	if len(items) != 1 {
		t.Fatalf("filed %d items, want 1; reply %q", len(items), got)
	}
	if !strings.Contains(got, "Filed bug **"+items[0].ID+"**") || !strings.Contains(got, "⚠️ Filed, but posting it in the channel failed") {
		t.Errorf("reply = %q", got)
	}
	if posts := h.filedPosts(); len(posts) != 0 {
		t.Errorf("%d announcements after a failed post, want 0", len(posts))
	}
}

// wi_bce6fceb4865c5b7: an announcement whose answer was lost is found in the
// channel, so the owner is not told it failed.
func TestBugAnnounceLostAnswerIsNotReported(t *testing.T) {
	h := newHarness(t)
	h.fake.loseCreateMessage = 1
	in := fileCommand("950000000000000801", "bug", "Channel answer lost. Post still lands.")
	got := editContent(h.interact(in))
	items := h.items()
	if len(items) != 1 {
		t.Fatalf("filed %d items, want 1; reply %q", len(items), got)
	}
	if !strings.Contains(got, "Filed bug **"+items[0].ID+"**") || strings.Contains(got, "posting it in the channel failed") {
		t.Errorf("reply = %q", got)
	}
	if posts := h.filedPosts(); len(posts) != 1 {
		t.Errorf("%d announcements after a lost answer, want 1", len(posts))
	}
}
