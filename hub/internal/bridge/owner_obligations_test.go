package bridge

import (
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/store"
	"strings"
	"testing"
	"time"
)

func TestOwnerCardFullTextApproveAndRetry(t *testing.T) {
	h := newHarness(t)
	item, err := h.st.CreateWorkItem(h.ctx, h.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Owner request fixture", RequestID: "owner-item"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	exact := " APPROVE 矩阵\nsha256:fixture\n "
	m := h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, RequestID: "owner-request", Envelope: &api.Envelope{Kind: "request", To: "owner", Subject: "Approve the full Unicode fixture request", ExpectedAnswer: exact, Body: api.EnvelopeBody{Ask: strings.Repeat("矩阵🙂", 350)}}, WorkItems: []api.MessageWorkItem{{ItemTaskID: h.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	// Rendered text exceeds a single embed and must survive multipart sends.
	h.fake.loseCreateMessage = 1
	h.cycle()
	h.makeDue()
	h.cycle()
	h.cycle()
	var rendered strings.Builder
	buttons := 0
	buttonID := ""
	for _, x := range h.lines() {
		if !strings.Contains(x.Content, "owner-request") {
			continue
		}
		for _, e := range x.Embeds {
			rendered.WriteString(e.Description)
		}
		for _, row := range x.Components {
			for _, b := range row.Components {
				if b.Label == "Approve" {
					buttons++
					buttonID = b.CustomID
				}
			}
		}
	}
	if rendered.String() != m.Text || buttons != 1 {
		t.Fatalf("full request or button lost: bytes %d/%d buttons %d", rendered.Len(), len(m.Text), buttons)
	}
	before := len(h.boardMessages())
	h.interact(discord.Interaction{ID: "910000000000000001", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttonID}, Member: &discord.Member{User: &discord.User{ID: strangerID}}})
	h.interact(discord.Interaction{ID: "910000000000000002", ChannelID: "unmapped", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttonID}})
	if len(h.boardMessages()) != before {
		t.Fatal("unauthorized click changed hub")
	}
	in := discord.Interaction{ID: "910000000000000003", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttonID}}
	got := editContent(h.interact(in))
	if !strings.Contains(got, "Approved") {
		t.Fatal(got)
	}
	h.interact(in)
	h.interact(discord.Interaction{ID: "910000000000000004", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: buttonID}})
	answers := 0
	for _, x := range h.boardMessages() {
		if x.ReplyTo == m.Seq {
			answers++
			if x.Envelope.Body.Answer != exact || x.Source == nil || x.Source.ID != in.ID || x.Source.UserID != ownerID {
				t.Fatalf("approval %+v", x)
			}
		}
	}
	if answers != 1 {
		t.Fatal(answers)
	}
}
func TestOwnerOverdueMentionOnce(t *testing.T) {
	h := newHarness(t)
	item, err := h.st.CreateWorkItem(h.ctx, h.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Escalation fixture", RequestID: "esc-item"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, RequestID: "esc-ask", Envelope: &api.Envelope{Kind: "request", Subject: "Answer the overdue owner fixture", Body: api.EnvelopeBody{Ask: "please"}}, WorkItems: []api.MessageWorkItem{{ItemTaskID: h.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	open, _ := h.st.BrokerOpenObligations(h.ctx)
	if err := h.st.BrokerEscalateOwnerRequest(h.ctx, open[0], open[0].DueAt); err != nil {
		t.Fatal(err)
	}
	h.fake.loseCreateMessage = 1
	h.cycle()
	h.makeDue()
	h.cycle()
	h.cycle()
	count := 0
	for _, x := range h.lines() {
		if strings.Contains(x.Content, "Owner request is past") {
			count++
			if !strings.Contains(x.Content, "<@"+ownerID+">") || x.AllowedMentions == nil || len(x.AllowedMentions.Users) != 1 {
				t.Fatalf("mention %+v", x)
			}
		}
	}
	if count != 1 {
		t.Fatalf("sent %d escalations", count)
	}
	list, _ := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{OwnerOnly: true}, time.Now())
	if len(list) != 1 {
		t.Fatal(fmt.Sprint(list))
	}
}
