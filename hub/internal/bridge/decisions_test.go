package bridge

import (
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

// a5: an owner's button click answers the decision once; a resumed
// interaction returns the same answer, another option then reads already
// answered, and a non-owner click changes nothing.
func TestDecisionButtonAnswersOnce(t *testing.T) {
	h := newHarness(t)
	d := h.decide(h.builder, "button", threeOptionDecision())
	h.cycle()
	buttons := cardButtons(h.decisionCards(d.Seq)[0])

	before := len(h.boardMessages())
	h.interact(discord.Interaction{ID: "950000000000000001", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttons[1].CustomID}, Member: &discord.Member{User: &discord.User{ID: strangerID}}})
	if len(h.boardMessages()) != before || h.decisionRecord(d.Seq).Answer != nil {
		t.Fatal("a non-owner click answered the decision")
	}

	in := discord.Interaction{ID: "950000000000000002", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttons[1].CustomID}}
	got := editContent(h.interact(in))
	rec := h.decisionRecord(d.Seq)
	if rec.Answer == nil || rec.Answer.DecisionAnswer == nil || rec.Answer.DecisionAnswer.OptionID != "b" || rec.Answer.ReplyTo != d.Seq || rec.Answer.To != h.builder.ID {
		t.Fatalf("stored answer = %+v", rec.Answer)
	}
	want := fmt.Sprintf("**owner** answered decision #%d: B · Restart at 03:00\n%s", d.Seq, marker(rec.Answer.Seq, 1, 1, ""))
	if got != want {
		t.Fatalf("click reply = %q, want %q", got, want)
	}
	// The resumed interaction (same ID) replays the stored answer.
	again := h.b.act(h.ctx, h.task.ID, in)
	if again.Content != want || !again.Public {
		t.Fatalf("replayed click = %+v, want %q", again, want)
	}
	afterAnswer := len(h.boardMessages())
	other := editContent(h.interact(discord.Interaction{ID: "950000000000000003", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttons[2].CustomID}}))
	if other != fmt.Sprintf("✔️ Already answered: decision #%d.", d.Seq) || len(h.boardMessages()) != afterAnswer {
		t.Fatalf("second option = %q, board %d → %d", other, afterAnswer, len(h.boardMessages()))
	}
	if r := h.decisionRecord(d.Seq); r.Answer.Seq != rec.Answer.Seq || r.Answer.DecisionAnswer.OptionID != "b" {
		t.Fatalf("answer changed: %+v", r.Answer)
	}
	// The bridge credential reads and answers decisions but can never ask one.
	_, err := h.b.cfg.Hub.CreateDecision(h.ctx, h.task.ID, api.CreateDecisionRequest{DecisionRequest: threeOptionDecision(), AgentID: h.builder.ID, RequestID: "bridge-ask"})
	var refused *api.HTTPError
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
		t.Fatalf("bridge create decision err = %v, want 403", err)
	}
	// The bridge's own answer is not mirrored back as a line.
	h.cycle()
	for _, m := range h.lines() {
		if hasLine(m.Content, marker(rec.Answer.Seq, 1, 1, "")) {
			t.Fatalf("answer mirrored: %q", m.Content)
		}
	}
}

// a6: /answer #<seq> takes a letter by position, an exact option ID or the
// owner's own words; an open obligation's /answer is unchanged.
func TestDecisionSlashAnswer(t *testing.T) {
	h := newHarness(t)
	cmd := func(id, ref, text string) string {
		return editContent(h.interact(discord.Interaction{ID: id, Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "answer",
			Options: []discord.InteractionOption{stringOption("obligation", ref), stringOption("text", text)}}}))
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
		{"letter past the options is own words", "e", threeOptionDecision(), "", "e", ": e"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := h.decide(h.builder, fmt.Sprintf("slash-%d", i), tc.req)
			got := cmd(fmt.Sprintf("96000000000000000%d", i), "#"+itoa(d.Seq), tc.text)
			rec := h.decisionRecord(d.Seq)
			if rec.Answer == nil || rec.Answer.DecisionAnswer == nil || rec.Answer.DecisionAnswer.OptionID != tc.option || rec.Answer.DecisionAnswer.Text != tc.own {
				t.Fatalf("stored answer = %+v", rec.Answer)
			}
			if !strings.HasPrefix(got, fmt.Sprintf("**owner** answered decision #%d: ", d.Seq)) || !strings.Contains(got, tc.replyContain) || !hasLine(got, marker(rec.Answer.Seq, 1, 1, "")) {
				t.Fatalf("/answer reply = %q", got)
			}
			if again := cmd(fmt.Sprintf("97000000000000000%d", i), itoa(d.Seq), "b"); again != fmt.Sprintf("✔️ Already answered: decision #%d.", d.Seq) {
				t.Fatalf("second /answer = %q", again)
			}
		})
	}

	// An open obligation keeps today's /answer.
	question := h.post(typed(h.builder, h.lead, api.EnvelopeKindQuestion, "Which fixture should the test use"))
	if got := cmd("980000000000000001", itoa(question.Seq), "Use the static fixture."); !strings.Contains(got, "answered #"+itoa(question.Seq)+" on the recipient's behalf") {
		t.Fatalf("/answer on an obligation = %q", got)
	}
	// A number that is neither keeps today's reply.
	if got := cmd("980000000000000002", "#999999", "a"); got != "✔️ Already handled: that obligation is closed or was moved." {
		t.Fatalf("/answer on nothing = %q", got)
	}
}
