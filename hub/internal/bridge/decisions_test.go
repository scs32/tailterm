package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// Structured owner asks in Discord (wi_be6e41aed949f387 a4–a6): a tt ask
// decision mirrors as a card with lettered options and a button for each,
// and an owner's click or /answer stores the decision answer. A real
// in-process hub and store, a fake Discord, no live owner data.

func (h *harness) decide(author api.Agent, key string, req api.DecisionRequest) api.Message {
	h.t.Helper()
	m, err := h.st.CreateDecision(h.ctx, h.task.ID, api.CreateDecisionRequest{DecisionRequest: req, AgentID: author.ID, RequestID: key}, owner)
	if err != nil {
		h.t.Fatalf("create decision: %v", err)
	}
	return m
}

func threeOptionDecision() api.DecisionRequest {
	return api.DecisionRequest{
		Question: "Restart the Mini tonight?",
		Options: []api.DecisionOption{
			{ID: "a", Label: "Restart at 22:00", Description: "Teams pause for ten minutes."},
			{ID: "b", Label: "Restart at 03:00", Description: "Nobody is working then."},
			{ID: "c", Label: "Do not restart", Description: "Memory pressure stays high."},
		},
		RecommendedOptionID:  "b",
		RecommendationReason: "The night slot interrupts nobody.",
	}
}

// decisionCards are the bot's lines for one decision, in send order.
func (h *harness) decisionCards(seq int64) []*fakeMessage {
	var out []*fakeMessage
	for _, m := range h.lines() {
		if n, ok := markerSeq(m.Content); ok && n == seq && strings.HasPrefix(m.Content, "**Decision for owner**") {
			out = append(out, m)
		}
	}
	return out
}

func cardButtons(m *fakeMessage) []discord.Component {
	var out []discord.Component
	for _, row := range m.Components {
		out = append(out, row.Components...)
	}
	return out
}

func (h *harness) decisionRecord(seq int64) api.DecisionRecord {
	h.t.Helper()
	list, err := h.st.ListDecisions(h.ctx, h.task.ID, seq-1, 1)
	if err != nil || len(list.Decisions) != 1 || list.Decisions[0].Request.Seq != seq {
		h.t.Fatalf("decision #%d: %+v %v", seq, list, err)
	}
	return list.Decisions[0]
}

// a4: the card, its letters in stored order, the recommendation, the answer
// line and one button per option; other messages render as before.
func TestDecisionCardLettersOptionsAndButtons(t *testing.T) {
	h := newHarness(t)
	plain := h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, Text: "Plain progress line."})
	d := h.decide(h.builder, "card", threeOptionDecision())
	h.cycle()

	cards := h.decisionCards(d.Seq)
	if len(cards) != 1 {
		t.Fatalf("decision cards = %d, want 1: %+v", len(cards), h.lines())
	}
	card := cards[0]
	if want := "**Decision for owner** from **builder-41b1c632**\n" + marker(d.Seq, 1, 1, "decision"); card.Content != want {
		t.Fatalf("card content = %q, want %q", card.Content, want)
	}
	if len(card.Embeds) != 1 {
		t.Fatalf("card embeds = %+v", card.Embeds)
	}
	want := "Restart the Mini tonight?\n\n" +
		"**A** · Restart at 22:00\nTeams pause for ten minutes.\n\n" +
		"**B** · Restart at 03:00 (Recommended)\nNobody is working then.\n\n" +
		"**C** · Do not restart\nMemory pressure stays high.\n\n" +
		"Recommendation: The night slot interrupts nobody.\n\n" +
		fmt.Sprintf("Answer with a button, or /answer #%d with a letter or your own words. A plain reply does not answer.", d.Seq)
	if got := card.Embeds[0].Description; got != want {
		t.Fatalf("card text =\n%s\nwant\n%s", got, want)
	}
	buttons := cardButtons(card)
	if len(card.Components) != 1 || len(buttons) != 3 {
		t.Fatalf("card controls = %+v", card.Components)
	}
	for i, id := range []string{"a", "b", "c"} {
		btn := buttons[i]
		style := discord.ButtonSecondary
		if id == "b" {
			style = discord.ButtonPrimary
		}
		if btn.CustomID != fmt.Sprintf("decide:%d:%s", d.Seq, id) || btn.Style != style || !strings.HasPrefix(btn.Label, decisionLetter(i)+" · ") {
			t.Fatalf("button %d = %+v", i, btn)
		}
	}

	// A message that is not a decision renders exactly as before.
	wantPlain := "**builder-41b1c632** → **everyone**\nPlain progress line.\n" + marker(plain.Seq, 1, 1, "")
	found := false
	for _, m := range h.lines() {
		if m.Content == wantPlain && len(m.Components) == 0 && len(m.Embeds) == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("plain message changed: %+v", h.lines())
	}
}

// a4: a long decision splits into parts; only the last carries the buttons,
// and button labels stay within Discord's limit.
func TestDecisionCardLongQuestionButtonsOnLastPart(t *testing.T) {
	h := newHarness(t)
	req := api.DecisionRequest{Question: strings.Repeat("Why the queue held overnight matters. ", 50), RecommendedOptionID: "o3", RecommendationReason: strings.Repeat("Because. ", 100)}
	for i := 1; i <= 5; i++ {
		req.Options = append(req.Options, api.DecisionOption{ID: fmt.Sprintf("o%d", i), Label: strings.Repeat(fmt.Sprintf("Label %d ", i), 15), Description: strings.Repeat("Detail line. ", 70)})
	}
	d := h.decide(h.builder, "long", req)
	h.cycle()
	var parts []*fakeMessage
	for _, m := range h.lines() {
		if _, seq, ok := strings.Cut(m.Content, "\n-# #"); ok && strings.HasPrefix(seq, itoa(d.Seq)+" ") {
			parts = append(parts, m)
		}
	}
	if len(parts) < 2 {
		t.Fatalf("long decision parts = %d, want 2 or more", len(parts))
	}
	for i, p := range parts {
		if !strings.HasPrefix(p.Content, "**Decision for owner**") || !hasLine(p.Content, marker(d.Seq, i+1, len(parts), "decision")) {
			t.Fatalf("part %d content = %q", i+1, p.Content)
		}
		if utf8.RuneCountInString(p.Embeds[0].Description) > embedBudget {
			t.Fatalf("part %d embed is %d runes", i+1, utf8.RuneCountInString(p.Embeds[0].Description))
		}
		buttons := cardButtons(p)
		if i < len(parts)-1 && len(buttons) != 0 {
			t.Fatalf("part %d has buttons: %+v", i+1, buttons)
		}
		if i == len(parts)-1 {
			if len(buttons) != 5 {
				t.Fatalf("last part buttons = %d, want 5", len(buttons))
			}
			for _, b := range buttons {
				if utf8.RuneCountInString(b.Label) > buttonLabelLimit || len(b.CustomID) > 100 {
					t.Fatalf("button over Discord limits: %+v", b)
				}
			}
		}
	}

	// wi_6ac93c7e030b5dad g3: /answer closes only the part with the buttons.
	h.interact(discord.Interaction{ID: "960000000000000101", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "answer",
		Options: []discord.InteractionOption{stringOption("obligation", itoa(d.Seq)), stringOption("text", "c")}}})
	last := parts[len(parts)-1]
	for i, p := range parts[:len(parts)-1] {
		if n := len(h.messageEdits(p.ID)); n != 0 {
			t.Fatalf("part %d edited %d times", i+1, n)
		}
	}
	edits := h.messageEdits(last.ID)
	mk := marker(d.Seq, len(parts), len(parts), "decision")
	if len(edits) != 1 || edits[0].Components == nil || len(*edits[0].Components) != 0 ||
		!strings.HasSuffix(last.Content, "\n✅ **Answered:** C · "+clean(req.Options[2].Label)+"\n"+mk) {
		t.Fatalf("last part after /answer = %q, edits %+v", last.Content, edits)
	}
}

// a4: an owner helper's decision is redacted in every field of its card.
func TestDecisionCardRedactsHelperSecrets(t *testing.T) {
	h := newHelperHarness(t)
	helper := h.register()
	// Assembled at runtime so no tracked file holds a token shape (the
	// tool-redact corpus check scans every tracked file).
	secret := "gh" + "p_" + strings.Repeat("abcdefghij", 3) + "0123456789"
	req := api.DecisionRequest{
		Question: "Rotate " + secret + " now?",
		Options: []api.DecisionOption{
			{ID: "a", Label: "Rotate " + secret, Description: "Uses " + secret + " once more."},
			{ID: "b", Label: "Wait", Description: "Keep it."},
		},
		RecommendedOptionID: "a", RecommendationReason: "The old " + secret + " leaked.",
	}
	d := h.decide(helper, "helper-secret", req)
	h.cycle()
	cards := h.decisionCards(d.Seq)
	if len(cards) != 1 {
		t.Fatalf("helper decision cards = %d", len(cards))
	}
	shown := cards[0].Content + cards[0].Embeds[0].Description
	for _, b := range cardButtons(cards[0]) {
		shown += b.Label + b.CustomID
	}
	if strings.Contains(shown, secret) || strings.Count(shown, redacted) < 5 {
		t.Fatalf("helper secret not redacted everywhere: %s", shown)
	}
}

// deferFlags are the flags of the deferral the bridge sent an interaction.
func (h *harness) deferFlags(id string) (int, bool) {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	for _, cb := range h.fake.callbacks {
		if cb.ID == id && cb.Response.Type == discord.ResponseDeferredMessage && cb.Response.Data != nil {
			return cb.Response.Data.Flags, true
		}
	}
	return 0, false
}

// messageEdits are the bot's edits of one channel message, in order.
func (h *harness) messageEdits(id string) []discord.MessageEdit {
	h.t.Helper()
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	var out []discord.MessageEdit
	for _, r := range h.fake.requests {
		if r.Method == http.MethodPatch && r.Path == "/channels/"+h.channel+"/messages/"+id {
			raw, _ := json.Marshal(r.Body)
			var e discord.MessageEdit
			if err := json.Unmarshal(raw, &e); err != nil {
				h.t.Fatalf("edit body: %v", err)
			}
			out = append(out, e)
		}
	}
	return out
}

// postsWith are the bot's channel messages that carry a marker line.
func (h *harness) postsWith(mk string) []*fakeMessage {
	var out []*fakeMessage
	for _, m := range h.lines() {
		if hasLine(m.Content, mk) {
			out = append(out, m)
		}
	}
	return out
}

// wantClosedCard checks the card's latest edit: the answer shown above its
// marker and no buttons left.
func (h *harness) wantClosedCard(card *fakeMessage, seq int64, what string, edits int) {
	h.t.Helper()
	got := h.messageEdits(card.ID)
	if len(got) != edits {
		h.t.Fatalf("card edits = %d, want %d: %+v", len(got), edits, got)
	}
	last := got[len(got)-1]
	want := "**Decision for owner** from **builder-41b1c632**\n✅ **Answered:** " + what + "\n" + marker(seq, 1, 1, "decision")
	if last.Content == nil || *last.Content != want || card.Content != want {
		h.t.Fatalf("closed card content = %v (now %q), want %q", last.Content, card.Content, want)
	}
	if last.Components == nil || len(*last.Components) != 0 {
		h.t.Fatalf("closed card components = %v, want none", last.Components)
	}
	if last.AllowedMentions == nil || len(last.AllowedMentions.Parse) != 0 {
		h.t.Fatalf("closed card mentions = %+v", last.AllowedMentions)
	}
}

// a5, wi_6ac93c7e030b5dad g1–g4: an owner's button click answers the
// decision once. The click defers ephemerally, the confirmation is posted
// once in the channel and the card is closed; a resumed interaction returns
// the same answer, another option then reads already answered (and closes
// the card with the stored answer), and a non-owner click changes nothing.
func TestDecisionButtonAnswersOnce(t *testing.T) {
	h := newHarness(t)
	d := h.decide(h.builder, "button", threeOptionDecision())
	h.cycle()
	card := h.decisionCards(d.Seq)[0]
	buttons := cardButtons(card)
	click := func(id, user string, button int) discord.Interaction {
		in := discord.Interaction{ID: id, Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttons[button].CustomID},
			GuildID: "guild", ChannelID: h.channel, Member: &discord.Member{User: &discord.User{ID: user}}, Token: "token-" + id}
		msg := card.Message // Discord sends the clicked message with the click
		in.Message = &msg
		return in
	}

	before, lines := len(h.boardMessages()), len(h.lines())
	h.interact(click("950000000000000001", strangerID, 1))
	if len(h.boardMessages()) != before || h.decisionRecord(d.Seq).Answer != nil || len(h.lines()) != lines || len(h.messageEdits(card.ID)) != 0 {
		t.Fatal("a non-owner click answered, posted or edited")
	}

	in := click("950000000000000002", ownerID, 1)
	got := editContent(h.interact(in))
	if flags, ok := h.deferFlags(in.ID); !ok || flags != discord.FlagEphemeral {
		t.Fatalf("click deferral flags = %d (sent %v), want ephemeral", flags, ok)
	}
	rec := h.decisionRecord(d.Seq)
	if rec.Answer == nil || rec.Answer.DecisionAnswer == nil || rec.Answer.DecisionAnswer.OptionID != "b" || rec.Answer.ReplyTo != d.Seq || rec.Answer.To != h.builder.ID {
		t.Fatalf("stored answer = %+v", rec.Answer)
	}
	mk := marker(rec.Answer.Seq, 1, 1, "")
	want := fmt.Sprintf("**owner** answered decision #%d: B · Restart at 03:00\n%s", d.Seq, mk)
	if got != want {
		t.Fatalf("click reply = %q, want %q", got, want)
	}
	// The channel sees the answer: one post, without mentions.
	posts := h.postsWith(mk)
	if len(posts) != 1 || posts[0].Content != want || posts[0].AllowedMentions == nil || len(posts[0].AllowedMentions.Parse) != 0 {
		t.Fatalf("channel confirmation = %+v, want one %q", posts, want)
	}
	h.wantClosedCard(card, d.Seq, "B · Restart at 03:00", 1)

	// The resumed interaction (same ID) replays the stored answer and finds
	// its channel post instead of posting again.
	again := h.b.act(h.ctx, h.task.ID, in)
	if again.Content != want || again.Public {
		t.Fatalf("replayed click = %+v, want the private %q", again, want)
	}
	if n := len(h.postsWith(mk)); n != 1 {
		t.Fatalf("channel confirmations after a replay = %d, want 1", n)
	}
	h.wantClosedCard(card, d.Seq, "B · Restart at 03:00", 2)

	// A click on another option (an open card in another client) is refused
	// and closes the card with the stored answer, not the clicked one.
	afterAnswer, lines := len(h.boardMessages()), len(h.lines())
	other := click("950000000000000003", ownerID, 2)
	got = editContent(h.interact(other))
	if got != fmt.Sprintf("✔️ Already answered: decision #%d.", d.Seq) || len(h.boardMessages()) != afterAnswer || len(h.lines()) != lines {
		t.Fatalf("second option = %q, board %d → %d, lines %d → %d", got, afterAnswer, len(h.boardMessages()), lines, len(h.lines()))
	}
	if flags, _ := h.deferFlags(other.ID); flags != discord.FlagEphemeral {
		t.Fatalf("already-answered deferral flags = %d, want ephemeral", flags)
	}
	h.wantClosedCard(card, d.Seq, "B · Restart at 03:00", 3)
	if r := h.decisionRecord(d.Seq); r.Answer.Seq != rec.Answer.Seq || r.Answer.DecisionAnswer.OptionID != "b" {
		t.Fatalf("answer changed: %+v", r.Answer)
	}
	// The bridge credential reads and answers decisions but can never ask one.
	_, err := h.b.cfg.Hub.CreateDecision(h.ctx, h.task.ID, api.CreateDecisionRequest{DecisionRequest: threeOptionDecision(), AgentID: h.builder.ID, RequestID: "bridge-ask"})
	var refused *api.HTTPError
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Fatalf("bridge create decision err = %v, want 403", err)
	}
	// The bridge's own answer is not mirrored back as a second line.
	h.cycle()
	if n := len(h.postsWith(mk)); n != 1 {
		t.Fatalf("answer lines after a mirror pass = %d, want only the confirmation", n)
	}
}

// a6, wi_6ac93c7e030b5dad g1–g4: /answer #<seq> takes a letter by position,
// an exact option ID or the owner's own words, posts the confirmation in the
// channel and closes the card it finds there; an open obligation's /answer
// is confirmed in the channel too. Usage and refusal replies stay private.
func TestDecisionSlashAnswer(t *testing.T) {
	h := newHarness(t)
	cmd := func(id, ref, text string) string {
		got := editContent(h.interact(discord.Interaction{ID: id, Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "answer",
			Options: []discord.InteractionOption{stringOption("obligation", ref), stringOption("text", text)}}}))
		if flags, ok := h.deferFlags(id); !ok || flags != discord.FlagEphemeral {
			t.Fatalf("/answer %s deferral flags = %d (sent %v), want ephemeral", id, flags, ok)
		}
		return got
	}
	// Option IDs out of letter order: the letter is the shown position.
	outOfOrder := api.DecisionRequest{Question: "Which lane?", Options: []api.DecisionOption{
		{ID: "c", Label: "Small", Description: "Three paths or fewer."},
		{ID: "a", Label: "Planned", Description: "More paths."},
		{ID: "b", Label: "Hold", Description: "Wait a day."},
	}, RecommendedOptionID: "a", RecommendationReason: "It has four paths."}
	cases := []struct {
		name, text   string
		req          api.DecisionRequest
		option, own  string
		replyContain string
	}{
		{"letter by position", "c", outOfOrder, "b", "", "C · Hold"},
		{"letter with a dot, any case", "A.", outOfOrder, "c", "", "A · Small"},
		{"exact option ID", "later", api.DecisionRequest{Question: "When?", Options: []api.DecisionOption{
			{ID: "now", Label: "Now", Description: "Today."}, {ID: "later", Label: "Later", Description: "Next week."}}, RecommendedOptionID: "now", RecommendationReason: "Sooner is cheaper."}, "later", "", "B · Later"},
		{"own words", "Restart only after the release lands.", threeOptionDecision(), "", "Restart only after the release lands.", "Restart only after the release lands."},
		{"letter past the options is own words", "e", threeOptionDecision(), "", "e", "e"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := h.decide(h.builder, fmt.Sprintf("slash-%d", i), tc.req)
			h.cycle()
			card := h.decisionCards(d.Seq)[0]
			got := cmd(fmt.Sprintf("96000000000000000%d", i), "#"+itoa(d.Seq), tc.text)
			rec := h.decisionRecord(d.Seq)
			if rec.Answer == nil || rec.Answer.DecisionAnswer == nil || rec.Answer.DecisionAnswer.OptionID != tc.option || rec.Answer.DecisionAnswer.Text != tc.own {
				t.Fatalf("stored answer = %+v", rec.Answer)
			}
			mk := marker(rec.Answer.Seq, 1, 1, "")
			want := fmt.Sprintf("**owner** answered decision #%d: %s\n%s", d.Seq, tc.replyContain, mk)
			if got != want {
				t.Fatalf("/answer reply = %q, want %q", got, want)
			}
			if posts := h.postsWith(mk); len(posts) != 1 || posts[0].Content != want {
				t.Fatalf("channel confirmation = %+v, want one %q", posts, want)
			}
			h.wantClosedCard(card, d.Seq, tc.replyContain, 1)
			lines := len(h.lines())
			if again := cmd(fmt.Sprintf("97000000000000000%d", i), itoa(d.Seq), "b"); again != fmt.Sprintf("✔️ Already answered: decision #%d.", d.Seq) {
				t.Fatalf("second /answer = %q", again)
			}
			if len(h.lines()) != lines {
				t.Fatal("an already answered /answer posted in the channel")
			}
			h.wantClosedCard(card, d.Seq, tc.replyContain, 2)
		})
	}

	// A usage error and a refusal reach only the owner: nothing is posted.
	d := h.decide(h.builder, "slash-usage", threeOptionDecision())
	h.cycle()
	lines := len(h.lines())
	if got := cmd("980000000000000003", itoa(d.Seq), " "); got != "Say what the answer is: a letter, an option or your own words." {
		t.Fatalf("/answer without text = %q", got)
	}
	h.interact(discord.Interaction{ID: "980000000000000004", Type: discord.InteractionCommand, Member: &discord.Member{User: &discord.User{ID: strangerID}},
		Data: discord.InteractionData{Name: "answer", Options: []discord.InteractionOption{stringOption("obligation", itoa(d.Seq)), stringOption("text", "a")}}})
	if len(h.lines()) != lines || h.decisionRecord(d.Seq).Answer != nil || len(h.messageEdits(h.decisionCards(d.Seq)[0].ID)) != 0 {
		t.Fatal("a usage error or a refusal posted, answered or edited")
	}

	// An open obligation's /answer is confirmed in the channel as well.
	question := h.post(typed(h.builder, h.lead, api.EnvelopeKindQuestion, "Which fixture should the test use"))
	got := cmd("980000000000000001", itoa(question.Seq), "Use the static fixture.")
	if !strings.Contains(got, "answered #"+itoa(question.Seq)+" on the recipient's behalf") {
		t.Fatalf("/answer on an obligation = %q", got)
	}
	lastLine := got[strings.LastIndex(got, "\n")+1:]
	if posts := h.postsWith(lastLine); !strings.HasPrefix(lastLine, "-# #") || len(posts) != 1 || posts[0].Content != got {
		t.Fatalf("obligation confirmation in the channel = %+v, want one %q", posts, got)
	}
	// A number that is neither keeps today's reply.
	if got := cmd("980000000000000002", "#999999", "a"); got != "✔️ Already handled: that obligation is closed or was moved." {
		t.Fatalf("/answer on nothing = %q", got)
	}
}

// slashAnswer runs /answer and returns the reply the owner was shown.
func (h *harness) slashAnswer(id, ref, text string) string {
	return editContent(h.interact(discord.Interaction{ID: id, Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "answer",
		Options: []discord.InteractionOption{stringOption("obligation", ref), stringOption("text", text)}}}))
}

// wi_6ac93c7e030b5dad review b1 (#59303): a crash after the hub stored an
// obligation /answer but before the channel post. The resumed interaction
// finds the obligation closed, replays the answer by its request ID and
// posts the confirmation once, by message number or obligation ID. A closed
// obligation answered elsewhere still reads "Already handled" and posts
// nothing.
func TestObligationSlashAnswerResumedAfterStore(t *testing.T) {
	h := newHarness(t)
	mapping, err := h.b.cfg.State.MappingByChannel(h.ctx, h.channel)
	if err != nil {
		t.Fatal(err)
	}
	const text = "Use the static fixture."
	for i, byID := range []bool{false, true} {
		question := h.post(typed(h.builder, h.lead, api.EnvelopeKindQuestion, fmt.Sprintf("Which fixture should test %d use", i)))
		o, err := h.b.obligation(h.ctx, h.task.ID, itoa(question.Seq))
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("98000000000000002%d", i)
		// The first run stored the answer, then the bridge died.
		stored, err := h.b.cfg.Hub.AnswerObligation(h.ctx, h.task.ID, o.ID, api.ObligationAnswerRequest{Text: text, RequestID: "discord-interaction-" + id})
		if err != nil || stored.Message == nil {
			t.Fatalf("store the answer: %+v %v", stored, err)
		}
		ref := "#" + itoa(question.Seq)
		if byID {
			ref = o.ID
		}
		in := discord.Interaction{ID: id, Type: discord.InteractionCommand, GuildID: "guild", ChannelID: h.channel, Token: "token-" + id,
			Member: &discord.Member{User: &discord.User{ID: ownerID}}, Data: discord.InteractionData{Name: "answer",
				Options: []discord.InteractionOption{stringOption("obligation", ref), stringOption("text", text)}}}
		mk := marker(stored.Message.Seq, 1, 1, "")
		want := fmt.Sprintf("**owner** answered #%d on the recipient's behalf:\n%s\n%s", question.Seq, text, mk)
		before := len(h.boardMessages())
		for run := 0; run < 2; run++ { // the resume, then a second resume
			h.b.runInteraction(h.ctx, mapping, in, true)
			if got := editContent(h.fake.lastEdit(in.Token)); got != want {
				t.Fatalf("byID=%v run %d: resumed reply = %q, want %q", byID, run, got, want)
			}
			if posts := h.postsWith(mk); len(posts) != 1 || posts[0].Content != want {
				t.Fatalf("byID=%v run %d: channel confirmation = %+v, want one %q", byID, run, posts, want)
			}
		}
		if len(h.boardMessages()) != before {
			t.Fatalf("byID=%v: the replay stored another message", byID)
		}
	}

	// Answered elsewhere (another request ID): nothing to replay.
	question := h.post(typed(h.builder, h.lead, api.EnvelopeKindQuestion, "Which fixture should the last test use"))
	o, err := h.b.obligation(h.ctx, h.task.ID, itoa(question.Seq))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.b.cfg.Hub.AnswerObligation(h.ctx, h.task.ID, o.ID, api.ObligationAnswerRequest{Text: "Answered in TailOS.", RequestID: "tailos-answer-1"}); err != nil {
		t.Fatal(err)
	}
	lines, board := len(h.lines()), len(h.boardMessages())
	for i, ref := range []string{itoa(question.Seq), o.ID} {
		if got := h.slashAnswer(fmt.Sprintf("98000000000000003%d", i), ref, text); got != "✔️ Already handled: that obligation is closed or was moved." {
			t.Fatalf("/answer %s answered elsewhere = %q", ref, got)
		}
	}
	if len(h.lines()) != lines || len(h.boardMessages()) != board {
		t.Fatal("an obligation answered elsewhere posted or stored again")
	}
}
